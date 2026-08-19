package localtools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type recTracker struct {
	starts atomic.Int64
}

func (r *recTracker) Start(_ context.Context, op, subject string, _ ...any) (
	func(error), func(string, any), func(),
) {
	r.starts.Add(1)
	return func(error) {}, func(string, any) {}, func() {}
}

func newWebTools(_ *testing.T, tracker libtracker.ActivityTracker) taskengine.ToolsRepo {
	return localtools.NewWebCaller(tracker)
}

func ctxWithPolicy(policy map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), localtools.WebToolsName, policy)
}

func execWeb(t *testing.T, ctx context.Context, h taskengine.ToolsRepo, tool string, args map[string]any) (any, taskengine.DataType, error) {
	t.Helper()
	return h.Exec(ctx, time.Now(), args, false, &taskengine.ToolsCall{Name: localtools.WebToolsName, ToolName: tool})
}

// TestUnit_WebTools_AllowlistVocabulary pins what an operator can express about this toolset: "*" admits it, "!name" removes it, a bare name grants exactly it, an empty allowlist grants nothing.
func TestUnit_WebTools_AllowlistVocabulary(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	supported, err := tools.Supports(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{localtools.WebToolsName}, supported,
		"Supports must report the toolset key alone; a bare verb name would be separately addressable and survive \"!%s\"", localtools.WebToolsName)

	// The universe PersistentRepo reports: an operator-registered toolset plus this one.
	universe := append([]string{"local_fs"}, supported...)

	got := taskengine.ExportedApplyAllowlist([]string{"*"}, universe)
	require.Equal(t, universe, got,
		"\"*\" must admit every connected toolset; the scope is a namespace, not a hidden exclusion")

	got = taskengine.ExportedApplyAllowlist([]string{"local_fs", localtools.WebToolsName}, universe)
	require.Equal(t, []string{"local_fs", localtools.WebToolsName}, got,
		"a bare name grants exactly it — that is what an agent.md declaring it emits")

	got = taskengine.ExportedApplyAllowlist([]string{"*", "!" + localtools.WebToolsName}, universe)
	require.Equal(t, []string{"local_fs"}, got,
		"\"!\"+the toolset name is how an operator drops exactly this toolset")

	require.Empty(t, taskengine.ExportedApplyAllowlist(nil, universe),
		"an empty allowlist grants nothing")
}

// TestUnit_WebTools_HITLGatesByURLHost drives the whole plumbing the toolset
// depends on rather than gating itself: the wrapper reads the toolset name off
// the call, the url argument reaches the policy as an evaluation arg, and a
// `when host` rule refuses before any request is built.
func TestUnit_WebTools_HITLGatesByURLHost(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy.json"), []byte(`{
		"default_action": "allow",
		"rules": [
			{"tools":"`+localtools.WebToolsName+`","tool":"*","action":"deny","when":[{"key":"url","op":"host","value":"127.0.0.1,localhost,169.254.169.254"}]}
		]
	}`), 0o644))
	policy := hitlservice.New(hitlservice.NewFSPolicySource(dir), webTestTenant,
		webPolicyKV{"hitl-policy.json"}, libtracker.NoopTracker{})

	gated := localtools.NewHITLWrapper(localtools.NewWebCaller(&recTracker{}), alwaysApprove, policy, nil)

	res, dt, err := gated.Exec(ctxWithPolicy(map[string]string{"_denied_hosts": ""}), time.Now(),
		map[string]any{"url": srv.URL}, false,
		&taskengine.ToolsCall{Name: localtools.WebToolsName, ToolName: "web_get"})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, res, "Denied by the active policy")
	require.EqualValues(t, 0, hits.Load(), "the gate must refuse before the toolset builds a request")

	// A host the rule does not name falls through to default_action and reaches
	// the toolset, whose own knobs — read from the same ToolsArgs context — answer.
	res, dt, err = gated.Exec(ctxWithPolicy(map[string]string{"_denied_hosts": "example.test"}), time.Now(),
		map[string]any{"url": "http://example.test/"}, false,
		&taskengine.ToolsCall{Name: localtools.WebToolsName, ToolName: "web_get"})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Contains(t, res, "tools_policies.native-web._denied_hosts")
}

const webTestTenant = "00000000-0000-0000-0000-000000000001"

type webPolicyKV struct{ name string }

func (f webPolicyKV) GetKV(_ context.Context, _ string, out any) error {
	if p, ok := out.(*string); ok {
		*p = f.name
	}
	return nil
}

// TestUnit_WebTools_HITLApprovalDenialNeverSends pins that a mutating verb is
// gated by the shared wrapper, not by anything the toolset does itself: a
// denied approval means no request was ever made.
func TestUnit_WebTools_HITLApprovalDenialNeverSends(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	gated := localtools.NewHITLWrapper(localtools.NewWebCaller(&recTracker{}), alwaysDeny, approvePolicy(), nil)

	res, _, err := gated.Exec(ctxWithPolicy(map[string]string{"_denied_hosts": ""}), time.Now(),
		map[string]any{"url": srv.URL, "body": map[string]any{"k": "v"}}, false,
		&taskengine.ToolsCall{Name: localtools.WebToolsName, ToolName: "web_post"})
	require.NoError(t, err)
	require.Equal(t, localtools.DenyMessage, res)
	require.EqualValues(t, 0, hits.Load())

	// Approved, the same call reaches the server unchanged.
	approved := localtools.NewHITLWrapper(localtools.NewWebCaller(&recTracker{}), alwaysApprove, approvePolicy(), nil)
	_, _, err = approved.Exec(ctxWithPolicy(map[string]string{"_denied_hosts": ""}), time.Now(),
		map[string]any{"url": srv.URL, "body": map[string]any{"k": "v"}}, false,
		&taskengine.ToolsCall{Name: localtools.WebToolsName, ToolName: "web_post"})
	require.NoError(t, err)
	require.EqualValues(t, 1, hits.Load())
}

// TestUnit_WebTools_PolicyArgsAreScopedToTheToolsetName pins the knob lookup to
// the registered name: args filed under the pre-revival name reach nothing, so
// a stale tools_policies block cannot silently keep configuring the toolset.
func TestUnit_WebTools_PolicyArgsAreScopedToTheToolsetName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tools := newWebTools(t, &recTracker{})
	u, _ := url.Parse(srv.URL)

	stale := taskengine.WithToolsArgs(context.Background(), "webtools",
		map[string]string{"_denied_hosts": u.Hostname()})
	_, _, err := execWeb(t, stale, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err, "args filed under the old name are not this toolset's policy")

	live := ctxWithPolicy(map[string]string{"_denied_hosts": u.Hostname()})
	res, _, err := execWeb(t, live, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err)
	require.Contains(t, res, "_denied_hosts")
}

func TestUnit_WebTools_WriteBodySchemaDeclaresType(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	schemas, err := tools.GetToolsForToolsByName(context.Background(), localtools.WebToolsName)
	require.NoError(t, err)

	writeTools := map[string]struct{}{
		"web_post":   {},
		"web_put":    {},
		"web_patch":  {},
		"web_delete": {},
	}
	for _, tool := range schemas {
		if _, ok := writeTools[tool.Function.Name]; !ok {
			continue
		}
		params, ok := tool.Function.Parameters.(map[string]any)
		require.True(t, ok, tool.Function.Name)
		props, ok := params["properties"].(map[string]any)
		require.True(t, ok, tool.Function.Name)
		body, ok := props["body"].(map[string]any)
		require.True(t, ok, tool.Function.Name)
		types, ok := body["type"].([]any)
		require.True(t, ok, tool.Function.Name)
		require.Contains(t, types, "string", tool.Function.Name)
		require.Contains(t, types, "object", tool.Function.Name)
		require.Contains(t, types, "array", tool.Function.Name)
	}
}

func TestUnit_WebTools_Get_ReturnsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "GET", r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"n":42}`))
	}))
	defer srv.Close()

	tracker := &recTracker{}
	tools := newWebTools(t, tracker)
	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})

	res, dt, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	m, ok := res.(map[string]any)
	require.True(t, ok, "expected map, got %T", res)
	require.Equal(t, true, m["ok"])
	require.EqualValues(t, 1, tracker.starts.Load(), "tracker.Start must fire once per call")
}

func TestUnit_WebTools_Get_ReturnsTextWhenNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	res, dt, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Equal(t, "hello world", res)
}

func TestUnit_WebTools_RejectsUnknownArgs(t *testing.T) {
	tools := newWebTools(t, &recTracker{})

	_, _, err := execWeb(t, context.Background(), tools, "web_get", map[string]any{
		"url":        "https://example.test/",
		"unexpected": true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown argument")
	require.Contains(t, err.Error(), "unexpected")
}

func TestUnit_WebTools_Get_DeniedHostBlocked(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	// No host is denied by default — host policy is opt-in; setting _denied_hosts blocks the call before the URL is ever contacted.
	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": "localhost"})
	res, dt, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": "http://localhost/"})
	require.NoError(t, err, "soft denial must be a string result, not an error")
	require.Equal(t, taskengine.DataTypeString, dt)
	msg, _ := res.(string)
	require.Contains(t, msg, "is denied by tools_policies.native-web._denied_hosts")
}

func TestUnit_WebTools_Get_AllowedHostsExclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	ctx := ctxWithPolicy(map[string]string{
		"_denied_hosts":  "",
		"_allowed_hosts": u.Hostname(),
	})
	tools := newWebTools(t, &recTracker{})

	_, _, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err)

	// Different host: blocked even though denied_hosts is empty.
	res, _, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": "http://other.example.invalid/"})
	require.NoError(t, err)
	msg, _ := res.(string)
	require.Contains(t, msg, "not in allowed hosts")
}

func TestUnit_WebTools_Get_SchemeBlocked(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	// Default _allowed_schemes = http,https. file:// must be blocked.
	res, _, err := execWeb(t, context.Background(), tools, "web_get", map[string]any{"url": "file:///etc/passwd"})
	require.NoError(t, err)
	msg, _ := res.(string)
	require.Contains(t, msg, "not in allowed schemes")
}

func TestUnit_WebTools_Post_RequestBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be hit when body exceeds the cap")
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{
		"_denied_hosts":           "",
		"_max_request_body_bytes": "16",
	})
	tools := newWebTools(t, &recTracker{})

	bigBody := strings.Repeat("x", 1024)
	_, _, err := execWeb(t, ctx, tools, "web_post", map[string]any{
		"url":  srv.URL,
		"body": bigBody,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "request body is")
}

func TestUnit_WebTools_Get_ResponseTruncatedAtCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("a", 4096)))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{
		"_denied_hosts":       "",
		"_max_response_bytes": "100",
	})
	tools := newWebTools(t, &recTracker{})

	res, dt, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	out := res.(string)
	require.Contains(t, out, "truncated to 100 bytes")
	require.True(t, strings.HasPrefix(out, strings.Repeat("a", 100)))
}

func TestUnit_WebTools_Get_RetriesOn5xx(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 2 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{
		"_denied_hosts":       "",
		"_max_attempts":       "3",
		"_initial_backoff_ms": "1",
		"_max_backoff_ms":     "1",
	})
	tools := newWebTools(t, &recTracker{})

	_, dt, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": srv.URL})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	require.EqualValues(t, 2, hits.Load(), "must hit server exactly twice (first 503, then 200)")
}

func TestUnit_WebTools_Get_NoRetryOn4xx(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(404)
		_, _ = w.Write([]byte("not here"))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{
		"_denied_hosts": "",
		"_max_attempts": "3",
	})
	tools := newWebTools(t, &recTracker{})

	_, _, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": srv.URL})
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 404")
	require.EqualValues(t, 1, hits.Load(), "4xx must not retry")
}

func TestUnit_WebTools_Get_HeadersAsObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "bar", r.Header.Get("X-Foo"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	_, _, err := execWeb(t, ctx, tools, "web_get", map[string]any{
		"url":     srv.URL,
		"headers": map[string]any{"X-Foo": "bar"},
	})
	require.NoError(t, err)
}

func TestUnit_WebTools_Get_HeadersAsJSONString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "bar", r.Header.Get("X-Foo"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	_, _, err := execWeb(t, ctx, tools, "web_get", map[string]any{
		"url":     srv.URL,
		"headers": `{"X-Foo":"bar"}`,
	})
	require.NoError(t, err)
}

func TestUnit_WebTools_Post_BodyMarshalsAndSends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "v", got["k"])
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	_, _, err := execWeb(t, ctx, tools, "web_post", map[string]any{
		"url":  srv.URL,
		"body": map[string]any{"k": "v"},
	})
	require.NoError(t, err)
}

// TestUnit_WebTools_Post_BodyFromCallArgs drives the declarative path: a `tools`
// task carries its arguments on the ToolsCall rather than in the input map, and
// the body has to reach the request from there like the url and the headers do.
func TestUnit_WebTools_Post_BodyFromCallArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "bar", r.Header.Get("X-Foo"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, `{"k":"v"}`, string(body), "a call-args body is sent as-is")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	// input is nil: everything the task declared sits on the call.
	_, _, err := tools.Exec(ctx, time.Now(), nil, false, &taskengine.ToolsCall{
		Name:     localtools.WebToolsName,
		ToolName: "web_post",
		Args: map[string]string{
			"url":     srv.URL,
			"headers": `{"X-Foo":"bar"}`,
			"body":    `{"k":"v"}`,
		},
	})
	require.NoError(t, err)
}

// TestUnit_WebTools_Get_SendsNoCallArgsBody is the other half of that contract:
// web_get and web_head declare no body argument, so a call that carries one
// anyway still sends a bodyless request.
func TestUnit_WebTools_Get_SendsNoCallArgsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Empty(t, string(body), "GET declares no body argument")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	_, _, err := tools.Exec(ctx, time.Now(), nil, false, &taskengine.ToolsCall{
		Name:     localtools.WebToolsName,
		ToolName: "web_get",
		Args:     map[string]string{"url": srv.URL, "body": `{"k":"v"}`},
	})
	require.NoError(t, err)
}

// TestUnit_WebTools_Head_ReturnsStatusAndHeaders pins that web_head answers with the status code and response headers, never the (always-empty) response body.
func TestUnit_WebTools_Head_ReturnsStatusAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "HEAD", r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Add("X-Multi", "one")
		w.Header().Add("X-Multi", "two")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})
	tools := newWebTools(t, &recTracker{})

	res, dt, err := execWeb(t, ctx, tools, "web_head", map[string]any{"url": srv.URL})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	head, ok := res.(localtools.WebHeadResult)
	require.Truef(t, ok, "web_head must answer with WebHeadResult, got %T (%v)", res, res)
	require.Equal(t, 200, head.Status)
	require.Equal(t, "application/json", head.Headers["Content-Type"])
	require.Equal(t, `"abc123"`, head.Headers["Etag"], "names are canonical, so ETag is keyed Etag")
	require.Equal(t, "one, two", head.Headers["X-Multi"], "a repeated header is joined with \", \"")

	// The engine hands DataTypeJSON to json.Marshal, so this is the text the model reads, and it must match the published schema.
	raw, err := json.Marshal(head)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"status":200`)

	docs, err := tools.(schemaRepo).GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	require.NoError(t, docs[localtools.WebToolsName].Validate(ctx))
	declared := variantByRequired(t, docs[localtools.WebToolsName].Components.Schemas["WebHeadResponse"].Value, "status")
	assertResultIsDeclared(t, "web_head", declared, head)
	require.NotNil(t, declared.Properties["headers"].Value.AdditionalProperties.Schema,
		"headers is published as a string map")
}

func TestUnit_WebTools_UnknownToolErrors(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	_, _, err := execWeb(t, context.Background(), tools, "web_obliterate", map[string]any{"url": "http://example.com/"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown tool")
}

func TestUnit_WebTools_SchemasPerVerb(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	for _, verb := range []string{"web_get", "web_head", "web_post", "web_put", "web_patch", "web_delete"} {
		ts, err := tools.GetToolsForToolsByName(context.Background(), verb)
		require.NoError(t, err, verb)
		require.Len(t, ts, 1, verb)
		require.Equal(t, verb, ts[0].Function.Name, fmt.Sprintf("expected verb-specific tool %s", verb))
	}
	// Bundle name returns all six.
	all, err := tools.GetToolsForToolsByName(context.Background(), localtools.WebToolsName)
	require.NoError(t, err)
	require.Len(t, all, 6)
}
