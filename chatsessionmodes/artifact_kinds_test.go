package chatsessionmodes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// For each first-party kind: round-trip a representative payload and assert
// the resulting system-message body contains the human-readable markers the
// LLM is expected to see. A malformed payload of the same kind must not error
// — it falls back to the flat JSON body.

func Test_FirstPartyArtifactKinds_Registered(t *testing.T) {
	t.Parallel()
	want := []ArtifactKind{
		ArtifactKindFileExcerpt,
		ArtifactKindTerminalOutput,
		ArtifactKindOpenFile,
		ArtifactKindPlanStep,
		ArtifactKindCommandOutput,
		ArtifactKindRuntimeState,
	}
	got := map[ArtifactKind]bool{}
	for _, k := range FirstPartyArtifactKinds() {
		got[k] = true
	}
	for _, k := range want {
		require.True(t, got[k], "kind %q must be registered", k)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func singleMessage(t *testing.T, kind ArtifactKind, payload json.RawMessage) string {
	t.Helper()
	msgs, err := BuildInjectedSystemMessages(&ContextPayload{
		Artifacts: []ContextArtifact{{Kind: string(kind), Payload: payload}},
	}, time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "system", msgs[0].Role)
	require.NotEmpty(t, msgs[0].ID)
	return msgs[0].Content
}

func TestRender_FileExcerpt(t *testing.T) {
	t.Parallel()
	body := singleMessage(t, ArtifactKindFileExcerpt,
		mustMarshal(t, FileExcerptPayload{Path: "pkg/foo.go", Text: "package foo\n", Truncated: true}))
	require.Contains(t, body, "[Context kind=file_excerpt]")
	require.Contains(t, body, "File excerpt from pkg/foo.go")
	require.Contains(t, body, "(truncated)")
	require.Contains(t, body, "package foo")
}

func TestRender_TerminalOutput(t *testing.T) {
	t.Parallel()
	body := singleMessage(t, ArtifactKindTerminalOutput,
		mustMarshal(t, TerminalOutputPayload{
			Command:    "ls -la",
			Output:     "total 8\ndrwx.. x\n",
			CapturedAt: "2026-04-14T00:00:00Z",
		}))
	require.Contains(t, body, "[Context kind=terminal_output]")
	require.Contains(t, body, "Recent terminal output")
	require.Contains(t, body, "Command: ls -la")
	require.Contains(t, body, "drwx.. x")
}

func TestRender_OpenFile(t *testing.T) {
	t.Parallel()
	body := singleMessage(t, ArtifactKindOpenFile,
		mustMarshal(t, OpenFilePayload{
			Path: "README.md", Text: "# Hello\n", LineFrom: 1, LineTo: 1,
		}))
	require.Contains(t, body, "[Context kind=open_file]")
	require.Contains(t, body, "currently open")
	require.Contains(t, body, "README.md")
	require.Contains(t, body, "(lines 1-1)")
	require.Contains(t, body, "# Hello")
}

func TestRender_PlanStep(t *testing.T) {
	t.Parallel()
	body := singleMessage(t, ArtifactKindPlanStep,
		mustMarshal(t, PlanStepPayload{
			PlanID:       "p1",
			Ordinal:      3,
			Description:  "Read manifest",
			Status:       "failed",
			FailureClass: "capacity",
			LastFailure:  "context too big",
		}))
	require.Contains(t, body, "[Context kind=plan_step]")
	require.Contains(t, body, "Plan step 3")
	require.Contains(t, body, "status: failed")
	require.Contains(t, body, "failure_class=capacity")
	require.Contains(t, body, "Description: Read manifest")
	require.Contains(t, body, "Last failure summary:")
}

func TestRender_CommandOutput(t *testing.T) {
	t.Parallel()
	exit := 0
	body := singleMessage(t, ArtifactKindCommandOutput,
		mustMarshal(t, CommandOutputPayload{
			Command:  "go test ./...",
			Output:   "ok\n",
			ExitCode: &exit,
		}))
	require.Contains(t, body, "[Context kind=command_output]")
	require.Contains(t, body, "Command: go test ./...")
	require.Contains(t, body, "(exit=0)")
	require.Contains(t, body, "ok")
}

func TestRender_RuntimeState(t *testing.T) {
	t.Parallel()
	body := singleMessage(t, ArtifactKindRuntimeState,
		mustMarshal(t, RuntimeStatePayload{
			Name: "token_usage",
			Data: json.RawMessage(`{"in":100,"out":42}`),
		}))
	require.Contains(t, body, "[Context kind=runtime_state]")
	require.Contains(t, body, "Captured runtime state: token_usage")
	require.Contains(t, body, `"in":100`)
}

func TestRender_UnknownKind_FallsBackToRawJSON(t *testing.T) {
	t.Parallel()
	// Unknown kind passes regex but has no registered renderer. Must not error —
	// must just pass the raw JSON through inside the header.
	raw := json.RawMessage(`{"hello":"world"}`)
	body := singleMessage(t, ArtifactKind("my_custom_kind"), raw)
	require.Contains(t, body, "[Context kind=my_custom_kind]")
	require.Contains(t, body, `"hello":"world"`)
}

func TestRender_MalformedPayload_FallsBackToRawJSON(t *testing.T) {
	t.Parallel()
	// A known kind with a payload that doesn't match its typed shape must not
	// error — the renderer returns the raw JSON so the LLM at least sees
	// something, and the adversarial client can't break the turn.
	raw := json.RawMessage(`{"not_a_file_excerpt":true}`)
	body := singleMessage(t, ArtifactKindFileExcerpt, raw)
	require.Contains(t, body, "[Context kind=file_excerpt]")
	require.Contains(t, body, "not_a_file_excerpt")
}

func TestBuild_RejectsOversizedPayload(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", maxArtifactPayloadBytes+100)
	raw := mustMarshal(t, FileExcerptPayload{Path: "a", Text: big})
	_, err := BuildInjectedSystemMessages(&ContextPayload{
		Artifacts: []ContextArtifact{{Kind: string(ArtifactKindFileExcerpt), Payload: raw}},
	}, time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum size")
}

func TestBuild_RejectsCumulativeOverflow(t *testing.T) {
	t.Parallel()
	// 4 payloads at slightly under the per-artifact cap each → total beats the
	// cumulative cap. Exercises the cumulative-byte path, not the per-artifact
	// path.
	payload := mustMarshal(t, FileExcerptPayload{Path: "a", Text: strings.Repeat("y", 20000)})
	arts := make([]ContextArtifact, 4)
	for i := range arts {
		arts[i] = ContextArtifact{Kind: string(ArtifactKindFileExcerpt), Payload: payload}
	}
	_, err := BuildInjectedSystemMessages(&ContextPayload{Artifacts: arts}, time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "total injected context")
}
