package gojatool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// recordingHost is the five-line fake the one-method HostToolCaller interface
// buys: no engine, no HITL wrapper, no database, and the bridge is still tested
// against exactly the contract the real path satisfies.
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

	// A TEXT result arrives wrapped: .text is the deliberate way to parse it,
	// and the wrapper is what makes the mis-parse impossible to do by accident
	// (see TestUnit_Bridge_TextResultsCannotBeMisparsedSilently).
	res := mustEval(t, sb, `host.tool("local_fs.read_file", {path: "README.md"}).text.split("\n").length`)
	if string(res.Value) != "2" {
		t.Fatalf("a text tool result did not arrive as ToolText: %s", res.Value)
	}

	// The address split is what the engine's ToolsCall needs: provider and tool,
	// separately, exactly as spelled by the script.
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

	// Arguments are plain data on both sides: a script mutating what it passed
	// cannot reach back into engine memory, because JSON is the only crossing.
	mustEval(t, sb, `const a = {path: "p"}; host.tool("local_fs.read_file", a); a.path = "mutated"; 1`)
	calls = host.recorded()
	if got := calls[len(calls)-1].args["path"]; got != "p" {
		t.Fatalf("a post-call script mutation reached the host: path = %v", got)
	}
}

func TestUnit_Bridge_HostErrorSurfacesAsAThrownException(t *testing.T) {
	// The host's message is a teaching error written for exactly this moment;
	// paraphrasing it would throw that away.
	const hostMsg = "local_fs: cannot read /etc/shadow: outside the allowed directory (recoverable: adjust parameters and retry)"
	sb := sandboxWithHost(t, HostFunc(func(_ context.Context, _, _ string, _ map[string]any) (any, error) {
		return nil, errors.New(hostMsg)
	}))

	// Uncaught: it ends the run, message intact.
	_, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "/etc/shadow"})`, 0)
	if err == nil {
		t.Fatal("a failing host call returned successfully")
	}
	if !strings.Contains(err.Error(), "outside the allowed directory") {
		t.Fatalf("the host message was not preserved: %q", err)
	}

	// Caught: an ordinary JS exception, so a script can react to it.
	res := mustEval(t, sb, `
		try { host.tool("local_fs.read_file", {path: "/etc/shadow"}); "no throw" }
		catch (e) { e instanceof Error ? e.message : "wrong type" }`)
	if !strings.Contains(string(res.Value), "outside the allowed directory") {
		t.Fatalf("caught error = %s", res.Value)
	}
}

// A Go panic raised by the host tool path must become an error, not a crash.
// Without the recover() at the exec boundary this test takes the test binary
// down: goja re-panics anything that is not one of its own exception types.
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

	// The sandbox is still usable afterwards — one bad tool does not poison it.
	if res := mustEval(t, sb, `1+1`); string(res.Value) != "2" {
		t.Fatalf("the sandbox did not survive the panic: %s", res.Value)
	}
}

// Depth is exactly one. This is the rule that keeps the sandbox from being a
// trampoline back into itself.
func TestUnit_Bridge_RecursionIntoGojaIsRefused(t *testing.T) {
	host := &recordingHost{}
	sb := sandboxWithHost(t, host)

	for _, name := range []string{
		ToolsProviderName + "." + ToolEval,
		ToolsProviderName + ".some_script_tool",
		// Unqualified attempts at this provider's own tools get the real reason
		// rather than a lecture about the address form.
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

	// The guard fires BEFORE the host is consulted: it is a structural rule, not
	// a consequence of what the host happens to answer.
	if calls := host.recorded(); len(calls) != 0 {
		t.Fatalf("the host was reached by a refused call: %+v", calls)
	}

	// Every other provider still goes through.
	mustEval(t, sb, `host.tool("gointel.go_describe", {symbol: "x"})`)
	if len(host.recorded()) != 1 {
		t.Fatal("a legitimate call was refused")
	}
}

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
			// Every refusal shows the working form.
			if !strings.Contains(err.Error(), "host.tool(") {
				t.Errorf("refusal does not show the call form: %q", err)
			}
		})
	}
	if calls := host.recorded(); len(calls) != 0 {
		t.Fatalf("a malformed call reached the host: %+v", calls)
	}
}

func TestUnit_Bridge_NoHostWiredIsTyped(t *testing.T) {
	sb := newTestSandbox(t) // Config{} — no Host

	_, err := eval(t, sb, `host.tool("local_fs.read_file", {path: "x"})`, 0)
	if !errors.Is(err, ErrHostUnavailable) {
		t.Fatalf("error = %v, want ErrHostUnavailable", err)
	}

	// SetHost closes the construction cycle: the same toolset works afterwards.
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

// stubRepo is a minimal taskengine.ToolsRepo, standing in for the engine's
// aggregate repo so the adapter's ToolsCall shape is asserted without an engine.
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

// The registration site's one-liner. The ToolsCall it builds is the same shape
// PersistentRepo.Exec dispatches on: Name is the PROVIDER, ToolName is the tool.
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

// A script cannot spin forever through host.tool. This used to be the deadline's
// job and is now maxHostCalls', because the deadline learned to stop while a host
// call is in flight (hostState.stopClock — the thing on the other side may be a
// human reading an approval card, and a 2s budget cannot outlast a person). The
// PROPERTY under test is unchanged and is the one that matters: the loop ends,
// promptly, with a refusal that says why.
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
	if made != maxHostCalls {
		t.Fatalf("%d host calls got through, want exactly the %d budget", made, maxHostCalls)
	}
	t.Logf("%d host calls in %s before the budget refused the next one", made, elapsed.Round(time.Millisecond))
}

// TestUnit_Bridge_ADeadlineDoesNotRunWhileAHumanIsReadingACard is the
// regression test for the bug that live use found: an approve-tier tool means an
// operator in front of an approval card, no operator answers in 2 seconds, and a
// deadline that ran through the wait killed every script that reached a gated
// tool — the headline capability, dead on arrival, and invisible to every unit
// test in the tree because none of them has a human in it.
func TestUnit_Bridge_ADeadlineDoesNotRunWhileAHumanIsReadingACard(t *testing.T) {
	const deadline = 150 * time.Millisecond
	// Five times the budget, spent the way a person spends it: parked inside one
	// host call, not computing.
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

	// And the budget still bounds COMPUTE after the wait: the clock resumes.
	_, err = eval(t, sb, `host.tool("local_fs.read_file", {path: "x"}); while(true){}`, deadline)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline — the clock did not restart after the host call", err)
	}
	// The message reports COMPUTE time, not the wall time a human spent.
	if strings.Contains(err.Error(), "spent 7") || strings.Contains(err.Error(), "spent 8") {
		t.Errorf("the deadline error is charging the script for the human's time: %v", err)
	}
}
