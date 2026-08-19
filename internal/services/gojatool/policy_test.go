package gojatool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
)

func policyCtx(name string, knobs map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), name, knobs)
}

// A chain-level policy reaches the sandbox through the context, under the name
// the toolset is registered as — the same channel local_fs and local_shell
// read theirs from.
func TestUnit_Policy_KnobsArriveUnderTheRegisteredName(t *testing.T) {
	ts, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	knobs := map[string]string{
		PolicyDeadlineMS:     "120",
		PolicyMaxDeadlineMS:  "400",
		PolicyMaxOutputBytes: "2048",
		PolicyMaxHostCalls:   "3",
	}

	lim := ts.sb.effective(policyCtx(ScopedToolsProviderName, knobs))
	if lim.deadline != 120*time.Millisecond {
		t.Errorf("deadline = %s, want the policy's 120ms", lim.deadline)
	}
	if lim.maxDeadline != 400*time.Millisecond {
		t.Errorf("maxDeadline = %s, want the policy's 400ms", lim.maxDeadline)
	}
	if lim.outputCap != 2048 {
		t.Errorf("outputCap = %d, want the policy's 2048", lim.outputCap)
	}
	if lim.maxHostCalls != 3 {
		t.Errorf("maxHostCalls = %d, want the policy's 3", lim.maxHostCalls)
	}

	// Keyed by the registration name, not by the package's bare identity: a
	// policy block written against the wrong key must not silently apply.
	if got := ts.sb.effective(policyCtx(ToolsProviderName, knobs)); got != ts.sb.base {
		t.Errorf("knobs stored under %q leaked into the %q toolset: %+v", ToolsProviderName, ScopedToolsProviderName, got)
	}
	if got := ts.sb.effective(context.Background()); got != ts.sb.base {
		t.Errorf("a chain with no policy did not get the defaults: %+v", got)
	}
}

// A knob only ever tightens: it cannot lift a hard ceiling, and a value that
// does not parse leaves the default standing rather than removing the bound.
func TestUnit_Policy_KnobsAreClampedAndFailClosed(t *testing.T) {
	ts, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	over := ts.sb.effective(policyCtx(ScopedToolsProviderName, map[string]string{
		PolicyMaxDeadlineMS:  "3600000",
		PolicyDeadlineMS:     "3600000",
		PolicyMaxOutputBytes: "1073741824",
		PolicyMaxHostCalls:   "1000000",
	}))
	if over.maxDeadline != MaxDeadline || over.deadline != MaxDeadline {
		t.Errorf("a policy raised the deadline past the %s ceiling: %+v", MaxDeadline, over)
	}
	if over.outputCap != maxOutputCap {
		t.Errorf("a policy raised the output cap past %d: %d", maxOutputCap, over.outputCap)
	}
	if over.maxHostCalls != maxHostCallsCeiling {
		t.Errorf("a policy raised the host-call budget past %d: %d", maxHostCallsCeiling, over.maxHostCalls)
	}

	for _, bad := range []string{"", "soon", "-1", "0", "12ms"} {
		got := ts.sb.effective(policyCtx(ScopedToolsProviderName, map[string]string{PolicyMaxHostCalls: bad}))
		if got.maxHostCalls != DefaultMaxHostCalls {
			t.Errorf("_max_host_calls=%q produced %d; a typo must leave the bound standing", bad, got.maxHostCalls)
		}
	}
}

// The knobs are not advisory: a policy deadline kills a spinning call, and a
// policy host-call budget stops a host.tool loop at exactly its number.
func TestUnit_Policy_KnobsBindTheExecution(t *testing.T) {
	var mu sync.Mutex
	var calls int
	ts, err := New(Config{Host: HostFunc(func(context.Context, string, string, map[string]any) (any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "x", nil
	})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	ctx := policyCtx(ScopedToolsProviderName, map[string]string{
		PolicyDeadlineMS:    "150",
		PolicyMaxDeadlineMS: "150",
		PolicyMaxHostCalls:  "4",
	})
	call := &taskengine.ToolsCall{Name: ScopedToolsProviderName, ToolName: ToolEval}

	start := time.Now()
	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{"code": `while(true){}`}, false, call)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want the policy deadline to have fired", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the policy deadline took %s to land", elapsed)
	}

	// The per-call override cannot climb over the policy's own ceiling either.
	start = time.Now()
	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{"code": `while(true){}`, "deadline_ms": 20000}, false, call)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a model-supplied deadline_ms escaped the policy ceiling: ran %s", elapsed)
	}

	_, _, err = ts.Exec(ctx, time.Now(), map[string]any{"code": `while(true){ host.tool("local_fs.read_file", {path: "x"}) }`}, false, call)
	if !errors.Is(err, ErrHostBudget) {
		t.Fatalf("error = %v, want ErrHostBudget", err)
	}
	mu.Lock()
	made := calls
	mu.Unlock()
	if made != 4 {
		t.Fatalf("%d host calls got through, want exactly the policy's 4", made)
	}
}

type fixedPolicy struct {
	action hitlservice.Action
	seen   []string
	mu     sync.Mutex
}

func (p *fixedPolicy) Evaluate(_ context.Context, toolsName, toolName string, _ map[string]any) (hitlservice.EvaluationResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, toolsName+"."+toolName)
	p.mu.Unlock()
	return hitlservice.EvaluationResult{Action: p.action, PolicyName: "test-policy"}, nil
}

func (p *fixedPolicy) evaluated() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// A script's host.tool call re-enters the engine's own HITL wrapper: the gate
// sees the address the script asked for, an allow reaches the inner repo, and
// nothing in this package decides the verdict.
func TestUnit_Policy_HostCallsPassTheEngineHITLGate(t *testing.T) {
	inner := &stubRepo{out: map[string]any{"branch": "main"}}
	policy := &fixedPolicy{action: hitlservice.ActionAllow}
	gate := localtools.NewHITLWrapper(inner, nil, policy, nil)

	ts, err := New(Config{Host: HostFromRepo(gate)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	res := mustEval(t, ts.sb, `host.tool("git.git_status", {path: "."}).branch`)
	if string(res.Value) != `"main"` {
		t.Fatalf("an approved call did not reach the inner repo: %s", res.Value)
	}
	if got := policy.evaluated(); len(got) != 1 || got[0] != "git.git_status" {
		t.Fatalf("the gate evaluated %v, want the address the script asked for", got)
	}
}

// Every shape of refusal the gate can produce arrives as a typed error, not as
// a sentence the script could go on to treat as data.
func TestUnit_Policy_ADeniedHostCallIsNotHandedToTheScriptAsData(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() taskengine.ToolsRepo
	}{
		{
			"denied by a policy rule",
			func() taskengine.ToolsRepo {
				return localtools.NewHITLWrapper(&stubRepo{out: "secret"}, nil, &fixedPolicy{action: hitlservice.ActionDeny}, nil)
			},
		},
		{
			"denied by the human at the card",
			func() taskengine.ToolsRepo {
				ask := func(context.Context, hitlservice.ApprovalRequest) (bool, error) { return false, nil }
				return localtools.NewHITLWrapper(&stubRepo{out: "secret"}, ask, &fixedPolicy{action: hitlservice.ActionApprove}, nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := New(Config{Host: HostFromRepo(tc.build())})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(ts.Shutdown)

			_, err = eval(t, ts.sb, `host.tool("local_fs.read_file", {path: "/etc/shadow"})`, 0)
			if err == nil {
				t.Fatal("a refusal came back as a value the script could report as success")
			}
			if !errors.Is(err, ErrToolDenied) {
				t.Fatalf("the refusal lost its sentinel: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("the gated tool's result leaked through a denial: %v", err)
			}

			// A script that expects the refusal can still carry on.
			res := mustEval(t, ts.sb, `
				let out;
				try { host.tool("local_fs.read_file", {path: "/etc/shadow"}); out = "read"; }
				catch (e) { out = "refused"; }
				out`)
			if got := string(res.Value); got != `"refused"` {
				t.Fatalf("a caught denial did not reach the script's handler: %s", got)
			}
		})
	}
}

// IsDenyMessage tracks the wrapper's own refusals rather than a copy of their
// wording: each string here is produced by the live gate, so a reworded
// refusal fails this test instead of quietly becoming script-visible data.
func TestUnit_Policy_DenyMessagesAreTheOnesTheGateActuallyProduces(t *testing.T) {
	ctx := context.Background()
	produced := map[string]any{}

	deny := localtools.NewHITLWrapper(&stubRepo{out: "x"}, nil, &fixedPolicy{action: hitlservice.ActionDeny}, nil)
	out, _, err := deny.Exec(ctx, time.Now(), map[string]any{}, false, &taskengine.ToolsCall{Name: "git", ToolName: "git_status"})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	produced["policy rule"] = out

	ask := func(context.Context, hitlservice.ApprovalRequest) (bool, error) { return false, nil }
	human := localtools.NewHITLWrapper(&stubRepo{out: "x"}, ask, &fixedPolicy{action: hitlservice.ActionApprove}, nil)
	out, _, err = human.Exec(ctx, time.Now(), map[string]any{}, false, &taskengine.ToolsCall{Name: "git", ToolName: "git_status"})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	produced["human"] = out
	produced["timeout"] = localtools.DenyTimeoutMessage

	for source, value := range produced {
		if !IsDenyMessage(value) {
			t.Errorf("the %s refusal is not recognised as a denial and would be handed to a script as data: %q", source, value)
		}
	}

	for _, notADenial := range []any{
		"the file body",
		map[string]any{"branch": "main"},
		nil,
		"Denied",
		42,
	} {
		if IsDenyMessage(notADenial) {
			t.Errorf("%#v was mistaken for a denial", notADenial)
		}
	}
}
