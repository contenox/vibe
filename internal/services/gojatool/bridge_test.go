package gojatool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

type recordingHost struct {
	mu    sync.Mutex
	calls []hostCall
	reply func(provider, tool string, args map[string]any) (any, error)
}

type hostCall struct {
	provider string
	tool     string
	args     map[string]any
}

func (h *recordingHost) CallTool(_ context.Context, provider, tool string, args map[string]any) (any, error) {
	h.mu.Lock()
	h.calls = append(h.calls, hostCall{provider: provider, tool: tool, args: args})
	h.mu.Unlock()
	if h.reply == nil {
		return "ok", nil
	}
	return h.reply(provider, tool, args)
}

func (h *recordingHost) recorded() []hostCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hostCall, len(h.calls))
	copy(out, h.calls)
	return out
}

func sandboxWithHost(t *testing.T, host HostToolCaller) *sandbox {
	t.Helper()
	ts, err := New(Config{Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)
	return ts.sb
}

// A text result arrives as ToolText, a structured result as a plain object, a
// nil result as null, and arguments cross by value only.
func TestUnit_Bridge_HostToolRoundTrip(t *testing.T) {
	host := &recordingHost{
		reply: func(provider, tool string, args map[string]any) (any, error) {
			switch tool {
			case "read_file":
				return "line one\nline two", nil
			case "stat_file":
				return map[string]any{"size": 12, "dir": false}, nil
			case "nothing":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected tool %s.%s", provider, tool)
		},
	}
	sb := sandboxWithHost(t, host)

	res := mustEval(t, sb, `host.tool("local_fs.read_file", {path: "README.md"}).text.split("\n").length`)
	if string(res.Value) != "2" {
		t.Fatalf("a text tool result did not arrive as ToolText: %s", res.Value)
	}

	calls := host.recorded()
	if len(calls) != 1 || calls[0].provider != "local_fs" || calls[0].tool != "read_file" {
		t.Fatalf("host saw %+v", calls)
	}
	if calls[0].args["path"] != "README.md" {
		t.Fatalf("arguments did not cross intact: %+v", calls[0].args)
	}

	res = mustEval(t, sb, `host.tool("local_fs.stat_file", {path: "x"}).size`)
	if string(res.Value) != "12" {
		t.Fatalf("a JSON tool result did not arrive as an object: %s", res.Value)
	}

	res = mustEval(t, sb, `host.tool("local_fs.nothing") === null`)
	if string(res.Value) != "true" {
		t.Fatalf("a nil tool result = %s, want null (and no arguments object is legal)", res.Value)
	}

	mustEval(t, sb, `const a = {path: "p"}; host.tool("local_fs.read_file", a); a.path = "mutated"; 1`)
	calls = host.recorded()
	if got := calls[len(calls)-1].args["path"]; got != "p" {
		t.Fatalf("a post-call script mutation reached the host: path = %v", got)
	}
}

// The host's error message is preserved verbatim, whether uncaught (ends the
// run) or caught by the script's own try/catch.
func TestUnit_Bridge_HostErrorSurfacesAsAThrownException(t *testing.T) {
	const hostMsg = "local_fs: cannot read /etc/shadow: outside the allowed directory (recoverable: adjust parameters and retry)"
	sb := sandboxWithHost(t, HostFunc(func(_ context.Context, _, _ string, _ map[string]any) (any, error) {
		return nil, errors.New(hostMsg)
	}))

	_, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "/etc/shadow"})`, 0)
	if err == nil {
		t.Fatal("a failing host call returned successfully")
	}
	if !strings.Contains(err.Error(), "outside the allowed directory") {
		t.Fatalf("the host message was not preserved: %q", err)
	}

	res := mustEval(t, sb, `
		try { host.tool("local_fs.read_file", {path: "/etc/shadow"}); "no throw" }
		catch (e) { e instanceof Error ? e.message : "wrong type" }`)
	if !strings.Contains(string(res.Value), "outside the allowed directory") {
		t.Fatalf("caught error = %s", res.Value)
	}
}

// A Go panic raised by the host tool path becomes an error, not a crash, and
// the sandbox stays usable afterwards.
func TestUnit_Bridge_HostPanicBecomesAnError(t *testing.T) {
	sb := sandboxWithHost(t, HostFunc(func(_ context.Context, _, _ string, _ map[string]any) (any, error) {
		panic("a tool with a bug dereferenced nil")
	}))

	_, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "x"})`, 0)
	if err == nil {
		t.Fatal("a panicking host tool returned successfully")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), "a tool with a bug") {
		t.Fatalf("error = %q, want the panic named", err)
	}
	if !strings.Contains(err.Error(), severityRecoverable) {
		t.Errorf("no severity marker: %q", err)
	}

	if res := mustEval(t, sb, `1+1`); string(res.Value) != "2" {
		t.Fatalf("the sandbox did not survive the panic: %s", res.Value)
	}
}

// A script may not call back into a goja-provider tool; depth is exactly one.
func TestUnit_Bridge_RecursionIntoGojaIsRefused(t *testing.T) {
	host := &recordingHost{}
	sb := sandboxWithHost(t, host)

	for _, name := range []string{
		ScopedToolsProviderName + "." + ToolEval,
		ScopedToolsProviderName + ".some_script_tool",
		ScopedToolsProviderName,
		// The bare name is refused too: a script must not find a back door by
		// addressing the toolset as it was called before it was scoped.
		ToolsProviderName + "." + ToolEval,
		ToolsProviderName + ".some_script_tool",
		ToolEval,
		ToolsProviderName,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := eval(t, sb, fmt.Sprintf(`host.tool(%q, {code: "1"})`, name), 0)
			if err == nil {
				t.Fatal("a recursive goja call was allowed")
			}
			if !errors.Is(err, ErrRecursionRefused) {
				t.Fatalf("error = %v, want ErrRecursionRefused", err)
			}
			if !strings.Contains(err.Error(), "depth is exactly one") {
				t.Errorf("error does not teach the rule: %q", err)
			}
		})
	}

	if calls := host.recorded(); len(calls) != 0 {
		t.Fatalf("the host was reached by a refused call: %+v", calls)
	}

	mustEval(t, sb, `host.tool("git.git_status", {path: "x"})`)
	if len(host.recorded()) != 1 {
		t.Fatal("a legitimate call was refused")
	}
}

// Malformed host.tool calls are refused before the host is reached, each with
// a teaching error naming the working call form.
func TestUnit_Bridge_MalformedCallsTeach(t *testing.T) {
	host := &recordingHost{}
	sb := sandboxWithHost(t, host)

	for _, tc := range []struct {
		name string
		code string
		want string
	}{
		{"no name", `host.tool()`, "needs a tool name"},
		{"empty name", `host.tool("")`, "needs a tool name"},
		{"no provider", `host.tool("read_file")`, "not a tool address"},
		{"leading dot", `host.tool(".read_file")`, "not a tool address"},
		{"trailing dot", `host.tool("local_fs.")`, "not a tool address"},
		{"a function as arguments", `host.tool("local_fs.read_file", () => 1)`, "plain JSON data"},
		{"a cycle in arguments", `const a={}; a.self=a; host.tool("local_fs.read_file", a)`, "plain JSON data"},
		{"a scalar as arguments", `host.tool("local_fs.read_file", 42)`, "must be an object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eval(t, sb, tc.code, 0)
			if err == nil {
				t.Fatalf("%s was accepted", tc.code)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not teach %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "host.tool(") {
				t.Errorf("refusal does not show the call form: %q", err)
			}
		})
	}
	if calls := host.recorded(); len(calls) != 0 {
		t.Fatalf("a malformed call reached the host: %+v", calls)
	}
}

// With no Host wired, host.tool fails typed; SetHost makes the same toolset
// work afterwards.
func TestUnit_Bridge_NoHostWiredIsTyped(t *testing.T) {
	sb := newTestSandbox(t)

	_, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "x"})`, 0)
	if !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("error = %v, want ErrHostUnavailable", err)
	}

	ts, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()
	ts.SetHost(HostFunc(func(_ context.Context, provider, tool string, _ map[string]any) (any, error) {
		return provider + "/" + tool, nil
	}))
	res := mustEval(t, ts.sb, `host.tool("local_fs.read_file").text`)
	if string(res.Value) != `"local_fs/read_file"` {
		t.Fatalf("after SetHost: %s", res.Value)
	}
}

type stubRepo struct {
	got *taskengine.ToolsCall
	in  any
	out any
	err error
}

func (r *stubRepo) Exec(_ context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	r.got, r.in = call, input
	return r.out, taskengine.DataTypeJSON, r.err
}
func (r *stubRepo) Supports(context.Context) ([]string, error) { return nil, nil }
func (r *stubRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}
func (r *stubRepo) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

// HostFromRepo builds a ToolsCall with Name as the provider and ToolName as
// the tool, and stays nil for a nil repo.
func TestUnit_Bridge_HostFromRepoBuildsTheEngineCall(t *testing.T) {
	repo := &stubRepo{out: map[string]any{"ok": true}}
	ts, err := New(Config{Host: HostFromRepo(repo)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ts.Shutdown()

	res := mustEval(t, ts.sb, `host.tool("local_fs.stat_file", {path: "go.mod"}).ok`)
	if string(res.Value) != "true" {
		t.Fatalf("value = %s", res.Value)
	}
	if repo.got == nil || repo.got.Name != "local_fs" || repo.got.ToolName != "stat_file" {
		t.Fatalf("ToolsCall = %+v, want {Name: local_fs, ToolName: stat_file}", repo.got)
	}
	args, ok := repo.in.(map[string]any)
	if !ok || args["path"] != "go.mod" {
		t.Fatalf("input = %#v, want the argument map the repo dispatches on", repo.in)
	}

	if HostFromRepo(nil) != nil {
		t.Error("HostFromRepo(nil) must stay nil so host.tool reports ErrHostUnavailable")
	}
}

// A host.tool loop is bounded by DefaultMaxHostCalls, not the deadline, and ends
// promptly with a refusal that says why.
func TestUnit_Bridge_HostCallLoopIsBounded(t *testing.T) {
	var calls int64
	var mu sync.Mutex
	sb := sandboxWithHost(t, HostFunc(func(_ context.Context, _, _ string, _ map[string]any) (any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(time.Millisecond)
		return "x", nil
	}))

	const deadline = 200 * time.Millisecond
	start := time.Now()
	_, err := eval(t, sb, `while(true){ host.tool("local_fs.read_file", {path: "x"}) }`, deadline)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrHostBudget) {
		t.Fatalf("error = %v, want ErrHostBudget", err)
	}
	if !strings.Contains(err.Error(), "pipeline, not a tool") {
		t.Errorf("the refusal does not teach the remedy: %v", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("a host-calling loop ran %s before it was stopped", elapsed)
	}
	mu.Lock()
	made := calls
	mu.Unlock()
	if made != DefaultMaxHostCalls {
		t.Fatalf("%d host calls got through, want exactly the %d budget", made, DefaultMaxHostCalls)
	}
	t.Logf("%d host calls in %s before the budget refused the next one", made, elapsed.Round(time.Millisecond))
}

// The deadline pauses while a script is parked inside one host.tool call.
func TestUnit_Bridge_ADeadlineDoesNotRunWhileAHumanIsReadingACard(t *testing.T) {
	const deadline = 150 * time.Millisecond
	const humanTime = 750 * time.Millisecond

	sb := sandboxWithHost(t, HostFunc(func(_ context.Context, _, _ string, _ map[string]any) (any, error) {
		time.Sleep(humanTime)
		return "the file body", nil
	}))

	res, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "x"}).bytes`, deadline)
	if err != nil {
		t.Fatalf("a script died at its %s deadline while waiting %s on a host call: %v", deadline, humanTime, err)
	}
	if got := string(res.Value); got != "13" {
		t.Fatalf("value = %s, want the length of the host's answer", got)
	}

	_, err = eval(t, sb, `host.tool("local_fs.read_file", {path: "x"}); while(true){}`, deadline)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline — the clock did not restart after the host call", err)
	}
	if strings.Contains(err.Error(), "spent 7") || strings.Contains(err.Error(), "spent 8") {
		t.Errorf("the deadline error is charging the script for the human's time: %v", err)
	}
}
