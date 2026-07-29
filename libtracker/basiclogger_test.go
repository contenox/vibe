package libtracker

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func testRedactor() *fieldRedactor { return newFieldRedactor(defaultRedactedFields) }

func TestUnit_BoundedLogValue_CapsLargePayloads(t *testing.T) {
	large := strings.Repeat("x", logChangeDataMaxJSONBytes+1024)

	got := boundedLogValue(large, testRedactor())

	summary, ok := got.(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, summary["truncated"])
	require.Equal(t, "payload_exceeds_limit", summary["reason"])
	require.Equal(t, "string", summary["original_type"])
	require.Greater(t, summary["original_json_bytes"], logChangeDataMaxJSONBytes)
	require.NotEmpty(t, summary["sha256"])
	require.LessOrEqual(t, summary["preview_bytes"], logChangeDataPreviewBytes)
}

func TestUnit_BoundedLogValue_KeepsSmallPayloads(t *testing.T) {
	got := boundedLogValue(map[string]any{"status": "ok"}, testRedactor())

	require.Equal(t, map[string]any{"status": "ok"}, got)
}

// A payload with no sensitive field names must come back as the *identical*
// value, not a JSON round-trip of it: redaction must not silently reshape the
// data every existing caller already logs.
func TestUnit_BoundedLogValue_NonSensitiveDataIsUntouched(t *testing.T) {
	type payload struct {
		User         string `json:"user"`
		MaxTokens    int    `json:"max_tokens"`
		PromptTokens int    `json:"prompt_tokens"`
		TokenCount   int    `json:"token_count"`
		Note         string `json:"note"`
	}
	in := payload{User: "alice", MaxTokens: 2048, PromptTokens: 17, TokenCount: 9, Note: "no secrets here"}

	got := boundedLogValue(in, testRedactor())

	require.Equal(t, in, got, "token accounting fields must not be mistaken for credentials")
}

func TestUnit_BoundedLogValue_RedactsNestedSecrets(t *testing.T) {
	in := map[string]any{
		"user": "alice",
		"auth": map[string]any{
			"API_Key":       "sk-live-should-never-be-logged",
			"user_password": "hunter2",
			"nested": []any{
				map[string]any{"Authorization": "Bearer abc.def.ghi"},
				map[string]any{"harmless": "keep me"},
			},
		},
		"config": map[string]any{
			"private-key": "-----BEGIN PRIVATE KEY-----",
			"session_id":  "s-123",
			"jwt":         "eyJhbGciOi",
			"timeout_ms":  1000,
		},
	}

	got := boundedLogValue(in, testRedactor())

	out, ok := got.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "alice", out["user"])

	auth := out["auth"].(map[string]any)
	require.Equal(t, redactedPlaceholder, auth["API_Key"])
	require.Equal(t, redactedPlaceholder, auth["user_password"])
	nested := auth["nested"].([]any)
	require.Equal(t, redactedPlaceholder, nested[0].(map[string]any)["Authorization"])
	require.Equal(t, "keep me", nested[1].(map[string]any)["harmless"])

	cfg := out["config"].(map[string]any)
	require.Equal(t, redactedPlaceholder, cfg["private-key"])
	require.Equal(t, redactedPlaceholder, cfg["session_id"])
	require.Equal(t, redactedPlaceholder, cfg["jwt"])
	require.EqualValues(t, 1000, cfg["timeout_ms"])

	// The secrets must not survive anywhere in the rendered form.
	rendered := fmt.Sprintf("%v", got)
	require.NotContains(t, rendered, "hunter2")
	require.NotContains(t, rendered, "sk-live-should-never-be-logged")
	require.NotContains(t, rendered, "Bearer abc.def.ghi")
}

// Struct payloads are reached through their JSON projection, so json-tagged
// names behave the way the log line will actually render them.
func TestUnit_BoundedLogValue_RedactsStructFields(t *testing.T) {
	type creds struct {
		Name  string `json:"name"`
		Token string `json:"api_token"`
	}
	type wrapper struct {
		Creds creds `json:"creds"`
	}

	rendered := fmt.Sprintf("%v", boundedLogValue(wrapper{Creds: creds{Name: "svc", Token: "t0ps3cret"}}, testRedactor()))

	require.NotContains(t, rendered, "t0ps3cret")
	require.Contains(t, rendered, "svc")
}

// The truncation summary is derived from post-redaction bytes: a secret must
// not survive inside the preview of a payload that was too big to log in full.
func TestUnit_BoundedLogValue_RedactsBeforeTruncating(t *testing.T) {
	in := map[string]any{
		"password": "hunter2",
		"filler":   strings.Repeat("y", logChangeDataMaxJSONBytes+1024),
	}

	summary, ok := boundedLogValue(in, testRedactor()).(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, summary["truncated"])
	require.NotContains(t, summary["preview"].(string), "hunter2")
}

func TestUnit_Redactor_HonoursCallerOverride(t *testing.T) {
	custom := newFieldRedactor(append(DefaultRedactedFields(), "ssn"))

	out := boundedLogValue(map[string]any{"ssn": "123-45-6789", "password": "x"}, custom).(map[string]any)
	require.Equal(t, redactedPlaceholder, out["ssn"])
	require.Equal(t, redactedPlaceholder, out["password"])

	// DefaultRedactedFields must hand back a copy, not the package's own slice.
	require.NotContains(t, DefaultRedactedFields(), "ssn")
}

// Deep nesting must fail closed rather than let a buried secret outrun the walk.
func TestUnit_Redactor_BoundsDepth(t *testing.T) {
	deep := any(map[string]any{"password": "hunter2"})
	for range maxRedactDepth + 5 {
		deep = map[string]any{"wrap": deep}
	}

	got, changed := testRedactor().redactValue(deep)
	require.True(t, changed)
	require.NotContains(t, fmt.Sprintf("%v", got), "hunter2")
}

func TestUnit_Tracker_RedactsKVArgsAndChangeData(t *testing.T) {
	var buf bytes.Buffer
	tracker := NewTextActivityTracker(&buf)

	_, reportChange, end := tracker.Start(context.Background(), "create", "user",
		"api_key", "sk-should-not-appear",
		"max_tokens", 2048,
		"payload", map[string]any{"secret": "also-not-here", "id": "u1"},
	)
	reportChange("u1", map[string]any{"token": "bearer-not-here", "name": "alice"})
	end()

	out := buf.String()
	require.NotContains(t, out, "sk-should-not-appear")
	require.NotContains(t, out, "also-not-here")
	require.NotContains(t, out, "bearer-not-here")
	require.Contains(t, out, "2048", "benign telemetry must survive")
	require.Contains(t, out, "alice")
}

func TestUnit_Tracker_DisablingRedactionIsPossible(t *testing.T) {
	var buf bytes.Buffer
	tracker := NewTextActivityTracker(&buf, WithRedactedFields())

	_, reportChange, end := tracker.Start(context.Background(), "create", "user")
	reportChange("u1", map[string]any{"password": "hunter2"})
	end()

	require.Contains(t, buf.String(), "hunter2")
}

var opIDPattern = regexp.MustCompile(`op_id=(\S+)`)

// op_id is the only handle correlating a single operation's log lines. A
// millisecond timestamp alone collides whenever two operations start in the
// same millisecond, silently merging their timelines.
func TestUnit_Tracker_OpIDsAreUniqueUnderConcurrency(t *testing.T) {
	var buf bytes.Buffer
	// slog handlers serialize writes to their writer, so one buffer is safe here.
	tracker := NewTextActivityTracker(&buf)

	const goroutines = 64
	const perGoroutine = 20

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				_, _, end := tracker.Start(context.Background(), "op", "subject")
				end()
			}
		}()
	}
	wg.Wait()

	seen := map[string]struct{}{}
	for _, m := range opIDPattern.FindAllStringSubmatch(buf.String(), -1) {
		seen[m[1]] = struct{}{}
	}
	require.Len(t, seen, goroutines*perGoroutine, "op_id collided across concurrent operations")
}
