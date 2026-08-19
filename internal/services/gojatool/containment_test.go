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

// gateSpyRepo stands in for the HITL-wrapped repo host.tool routes into: it
// records the tool-call identity and approval verdict the nested call arrives
// with, exactly as localtools.HITLWrapper reads them, so a test can prove the
// script faces the gate on its own footing.
type gateSpyRepo struct {
	mu        sync.Mutex
	sawID     string
	inherited bool
	reply     func() (any, taskengine.DataType, error)
}

func (r *gateSpyRepo) Exec(ctx context.Context, _ time.Time, _ any, _ bool, _ *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	id, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)
	approved, ok := taskengine.ApprovalVerdictFromContext(ctx, id)
	r.mu.Lock()
	r.sawID = id
	r.inherited = ok && approved
	r.mu.Unlock()
	if r.reply != nil {
		return r.reply()
	}
	return "ok", taskengine.DataTypeString, nil
}

func (r *gateSpyRepo) Supports(context.Context) ([]string, error) { return nil, nil }
func (r *gateSpyRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return nil, nil
}
func (r *gateSpyRepo) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

func (r *gateSpyRepo) snapshot() (id string, inherited bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawID, r.inherited
}

func hostedToolset(t *testing.T, repo taskengine.ToolsRepo) *Toolset {
	t.Helper()
	ts, err := New(Config{Host: HostFromRepo(repo)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)
	return ts
}

func runWith(t *testing.T, ts *Toolset, ctx context.Context, code string) (*Result, error) {
	t.Helper()
	return ts.sb.runSource(ctx, ToolEval, code, ts.sb.base, ts.sb.base.clampDeadline(0))
}

// A script runs under the outer goja tool call's context, which on a resumed run
// carries that call's own approval verdict keyed by its ID. A nested host.tool
// call must NOT ride it: the gate re-evaluates the nested call, so the sandbox
// cannot escape the envelope by inheriting the approval granted to goja_eval.
func TestUnit_Goja_Reconnect_NestedCallReentersGate(t *testing.T) {
	repo := &gateSpyRepo{}
	ts := hostedToolset(t, repo)

	const outerID = "outer-goja-eval-call"
	outer := context.WithValue(context.Background(), taskengine.ContextKeyToolCallID, outerID)
	outer = taskengine.WithApprovalVerdicts(outer, map[string]bool{outerID: true})

	// Precondition: the outer approval really is visible on this context, so a
	// pass below means the bridge severed it, not that it was never there.
	if _, ok := taskengine.ApprovalVerdictFromContext(outer, outerID); !ok {
		t.Fatal("test setup: the outer approval verdict is not on the context")
	}

	if _, err := runWith(t, ts, outer, `host.tool("local_fs.write_file", {path: "x", content: "y"})`); err != nil {
		t.Fatalf("eval: %v", err)
	}

	sawID, inherited := repo.snapshot()
	if inherited {
		t.Fatal("the nested host.tool call inherited goja_eval's own approval verdict; the gate was not authoritative over the script")
	}
	if sawID == outerID {
		t.Fatalf("the nested call carried the outer tool-call ID %q, so the wrapper would match it to the script's approval", outerID)
	}
}

// The gate answers a control-plane refusal with a policy-deny sentence written
// for a human; the script must be refused (ErrToolDenied), never handed that
// sentence as a value it could act on.
func TestUnit_Goja_Reconnect_ControlPlaneDenialIsNotData(t *testing.T) {
	repo := &gateSpyRepo{reply: func() (any, taskengine.DataType, error) {
		return "Denied by the active policy default (rule 3). Path is inside the runtime's control plane.", taskengine.DataTypeString, nil
	}}
	ts := hostedToolset(t, repo)

	_, err := runWith(t, ts, context.Background(), `host.tool("local_fs.read_file", {path: "/ctrl/policies.db"})`)
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("a control-plane denial did not refuse the script: %v", err)
	}
}

// A containment escape refused downstream (a vfs ErrEscape-shaped error) reaches
// the script as a thrown error, not as data — the script cannot read a file the
// gated tool would not open.
func TestUnit_Goja_Reconnect_EscapeRefusalSurfacesAsError(t *testing.T) {
	repo := &gateSpyRepo{reply: func() (any, taskengine.DataType, error) {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path escapes workspace root (recoverable: adjust parameters and retry)")
	}}
	ts := hostedToolset(t, repo)

	res, err := runWith(t, ts, context.Background(), `host.tool("local_fs.read_file", {path: "../../etc/shadow"})`)
	if err == nil {
		t.Fatalf("an escape refusal was handed back as data: %s", res.Value)
	}
	if !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("the containment refusal was not preserved: %v", err)
	}
}

// The sandbox's only reach is host.tool: it launches no process and reads no
// ambient environment, so the process env-scrub obligation does not arise in
// this package — it lives in the gated tools the bridge fronts, and in the
// surfaces that actually spawn. A script therefore has no process/spawn global
// to leak an unscrubbed environment through.
func TestUnit_Goja_Reconnect_SandboxLaunchesNoProcessToScrub(t *testing.T) {
	sb := newTestSandbox(t)
	for _, ambient := range []string{"process", "require", "module", "exports", "global", "Deno", "child_process", "Bun"} {
		res := mustEval(t, sb, fmt.Sprintf("typeof %s", ambient))
		if string(res.Value) != `"undefined"` {
			t.Fatalf("the sandbox exposes %q (%s); a script could reach an unscrubbed host environment directly", ambient, res.Value)
		}
	}
}

// The name Supports reports is the namespaced one, so an operator addresses the
// reconnected toolset by exactly that name: "*" admits it, "!name" removes it,
// and no declared MCP source can mint the same key.
func TestUnit_Goja_Reconnect_SupportsReportsTheNamespacedName(t *testing.T) {
	ts, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)

	names, err := ts.Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("Supports returned no names")
	}
	if names[0] != ScopedToolsProviderName {
		t.Fatalf("Supports()[0] = %q, want the scoped provider name %q", names[0], ScopedToolsProviderName)
	}

	reached := func(allow []string) bool {
		for _, n := range taskengine.ExportedApplyAllowlist(allow, names) {
			if n == ScopedToolsProviderName {
				return true
			}
		}
		return false
	}
	if !reached([]string{"*"}) {
		t.Fatal("a wildcard host did not reach the scoped toolset; the scope is a namespace, not a hidden exclusion")
	}
	if reached([]string{"*", "!" + ScopedToolsProviderName}) {
		t.Fatal("\"!\"+the toolset name did not remove it from under the wildcard")
	}
	if !reached([]string{ScopedToolsProviderName}) {
		t.Fatal("an exact declaration did not admit the scoped toolset")
	}
	if reached(nil) {
		t.Fatal("an empty allowlist admitted the scoped toolset")
	}
}
