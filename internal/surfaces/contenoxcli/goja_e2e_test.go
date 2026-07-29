package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// goja on the engine path: asserts a script's host.tool call meets the same
// envelope a model call would, as wired by BuildEngine into SetHost.
// Assertions read the engine's hitl_decision events rather than green
// results, since a bypass here is invisible to every other test in the tree.
// ---------------------------------------------------------------------------

// gojaAsker answers approval requests by rule and records every one it saw. The
// recording is the evidence; the answer is what lets a test drive both sides of
// a gate.
type gojaAsker struct {
	mu     sync.Mutex
	asks   []hitlservice.ApprovalRequest
	answer func(hitlservice.ApprovalRequest) bool
}

func (a *gojaAsker) ask(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, req)
	if a.answer == nil {
		return true, nil
	}
	return a.answer(req), nil
}

// seen returns the "<provider>.<tool>" of every ask since the last reset.
func (a *gojaAsker) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.asks))
	for _, r := range a.asks {
		out = append(out, r.ToolsName+"."+r.ToolName)
	}
	return out
}

func (a *gojaAsker) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = nil
}

// gojaSink captures the engine's HITL decisions — the only honest evidence of
// which envelope evaluated a call and what it decided. A green tool result
// cannot tell an allow from a gate that was never consulted.
type gojaSink struct {
	mu     sync.Mutex
	events []taskengine.TaskEvent
}

func (s *gojaSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *gojaSink) Wants(kind taskengine.TaskEventKind) bool {
	return kind == taskengine.TaskEventHITLDecision || kind == taskengine.TaskEventApprovalRequested
}

func (s *gojaSink) drain() []taskengine.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events = nil
	return out
}

// decisions reduces the captured events to "<provider>.<tool>=<action>" pairs,
// in order. This is the shape every assertion in this file reads.
func (s *gojaSink) decisions() []string {
	out := []string{}
	for _, ev := range s.drain() {
		if ev.Kind != taskengine.TaskEventHITLDecision {
			continue
		}
		out = append(out, fmt.Sprintf("%s.%s=%s", ev.HookName, ev.ToolName, ev.HITLAction))
	}
	return out
}

// --- the example scripts -----------------------------------------------------
//
// Written as an operator would write them, into $CONTENOX_DIR/tools: a
// descriptor, a run function, no ceremony.

const scriptStatsSummary = `// A pure-compute script tool: no host.tool, no I/O, just arithmetic the model
// should never be doing token-by-token.
const tool = {
  name: "stats_summary",
  description: "Summarise a comma-separated list of numbers: count, sum, mean, min, max, median.",
  schema: {
    type: "object",
    properties: { numbers: { type: "string", description: "Comma-separated numbers, e.g. \"3, 1, 4, 1, 5\"." } },
    required: ["numbers"],
  },
};

function run(args) {
  const xs = String(args.numbers)
    .split(",")
    .map(function (s) { return Number(s.trim()); })
    .filter(function (n) { return !isNaN(n); });
  if (xs.length === 0) throw new Error("no numbers found in: " + args.numbers);
  const sorted = xs.slice().sort(function (a, b) { return a - b; });
  const sum = xs.reduce(function (a, b) { return a + b; }, 0);
  const mid = Math.floor(sorted.length / 2);
  return {
    count: xs.length,
    sum: sum,
    mean: sum / xs.length,
    min: sorted[0],
    max: sorted[sorted.length - 1],
    median: sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2,
  };
}
`

const scriptFileOutline = `// A script tool that REACHES THE WORLD, through the one seam there is.
// local_fs.read_file is an allow-tier tool, so this must never raise a card.
// It DECLARES what it reaches, which is what an approval card for this script
// can show an operator before they answer.
const tool = {
  name: "file_outline",
  description: "Outline a text file: line count, byte count, and its Markdown headings or Go declarations.",
  schema: {
    type: "object",
    properties: { path: { type: "string", description: "Workspace-relative path to read." } },
    required: ["path"],
  },
  tools: ["local_fs.read_file"],
};

function run(args) {
  const text = host.tool("local_fs.read_file", { path: args.path }).text;
  const lines = String(text).split("\n");
  const marks = [];
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    if (/^#{1,6}\s/.test(l) || /^(func|type)\s/.test(l)) {
      marks.push({ line: i + 1, text: l.slice(0, 120) });
    }
  }
  return { path: args.path, lines: lines.length, bytes: String(text).length, marks: marks.slice(0, 50) };
}
`

const scriptNoteAppend = `// A script tool that MUTATES, through the same seam. local_fs.write_file is an
// approve-tier tool, so this must raise a card for local_fs.write_file — the
// script's own approval does not buy it.
const tool = {
  name: "note_append",
  description: "Write a timestamped note to a file in the workspace.",
  schema: {
    type: "object",
    properties: {
      path: { type: "string", description: "Workspace-relative path to write." },
      text: { type: "string", description: "The note body." },
    },
    required: ["path", "text"],
  },
  tools: ["local_fs.write_file"],
};

function run(args) {
  const body = "- " + args.text + "\n";
  const res = host.tool("local_fs.write_file", { path: args.path, content: body });
  return { wrote: args.path, host_said: res };
}
`

const scriptRecursionProbe = `// The recursion guard, exercised from the place it exists to stop.
const tool = {
  name: "recursion_probe",
  description: "Test-only: tries to call a goja-provider tool from inside a script.",
  schema: { type: "object", properties: {} },
};

function run() {
  return host.tool("goja.goja_eval", { code: "1 + 1" });
}
`

const scriptSlowLoop = `// A script that never finishes, to prove the deadline is the script's bound too.
const tool = {
  name: "slow_loop",
  description: "Test-only: spins forever.",
  schema: { type: "object", properties: {} },
  deadline_ms: 200,
};

function run() {
  while (true) {}
}
`

const scriptDeniedRead = `// Calls an approve-tier tool and USES the answer as if it were data. This is
// the script an author writes without thinking about denial.
const tool = {
  name: "denied_probe",
  description: "Test-only: writes a file and reports the length of whatever came back.",
  schema: { type: "object", properties: { path: { type: "string", description: "Path to write." } }, required: ["path"] },
  tools: ["local_fs.write_file"],
};

function run(args) {
  const res = host.tool("local_fs.write_file", { path: args.path, content: "x\n" });
  return { kind: typeof res, length: String(res).length, value: res };
}
`

// --- harness -----------------------------------------------------------------

type gojaHarness struct {
	engine      *Engine
	asker       *gojaAsker
	sink        *gojaSink
	root        string
	contenoxDir string
	scriptDir   string
}

// newGojaHarness builds a real engine through BuildEngine with HITL on, the
// shipped policy presets seeded into an isolated .contenox dir, and the
// example scripts in the tools/ directory BuildEngine loads from.
func newGojaHarness(t *testing.T, scripts map[string]string, answer func(hitlservice.ApprovalRequest) bool) *gojaHarness {
	t.Helper()

	// A gate that reaches for a TTY must fail at once, not park the test.
	devnull, err := os.Open(os.DevNull)
	require.NoError(t, err)
	realStdin := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() {
		os.Stdin = realStdin
		_ = devnull.Close()
	})

	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, writeEmbeddedHITLPolicies(contenoxDir, true))

	scriptDir := filepath.Join(contenoxDir, "tools")
	require.NoError(t, os.MkdirAll(scriptDir, 0o755))
	for name, body := range scripts {
		require.NoError(t, os.WriteFile(filepath.Join(scriptDir, name), []byte(body), 0o644))
	}

	// The workspace the tools are scoped to; note_append writes into it.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("# fixture\n\nsome text\n\n## second heading\n\nmore text\n"), 0o644))

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "goja-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	asker := &gojaAsker{answer: answer}
	sink := &gojaSink{}

	engine, err := BuildEngine(ctx, db, chatOpts{
		EffectiveDefaultModel:        "goja-e2e-fake-model",
		EffectiveDefaultProvider:     "ollama",
		EffectiveContext:             4096,
		EffectiveHITL:                true,
		EffectiveLocalExecAllowedDir: root,
		EffectiveSkipBackendCycle:    true,
		EffectiveAskApproval:         asker.ask,
		EffectiveTaskEventSink:       sink,
		ContenoxDir:                  contenoxDir,
	})
	require.NoError(t, err)

	h := &gojaHarness{engine: engine, asker: asker, sink: sink, root: root, contenoxDir: contenoxDir, scriptDir: scriptDir}
	t.Cleanup(h.stop)
	return h
}

func (h *gojaHarness) stop() {
	if h.engine != nil && h.engine.Stop != nil {
		h.engine.Stop()
		h.engine.Stop = nil
	}
}

// call runs one goja tool through the engine's chain executor — real dispatch,
// real HITL wrapper, shipped policy on disk.
func (h *gojaHarness) call(ctx context.Context, tool string, args map[string]string) (any, error) {
	chain := &taskengine.TaskChainDefinition{
		ID:          "goja-e2e",
		Description: "one goja tool call through the real engine",
		Tasks: []taskengine.TaskDefinition{{
			ID:      "call",
			Handler: taskengine.HandleTools,
			Tools: &taskengine.ToolsCall{
				Name:     gojatool.ToolsProviderName,
				ToolName: tool,
				Args:     args,
			},
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: taskengine.TermEnd}},
			},
		}},
	}
	out, _, _, err := h.engine.TaskService.Execute(ctx, chain, map[string]any{}, taskengine.DataTypeJSON)
	return out, err
}

// eval is the goja_eval shorthand, returning the decoded value.
func (h *gojaHarness) eval(t *testing.T, ctx context.Context, code string) (*gojatool.Result, any) {
	t.Helper()
	out, err := h.call(ctx, gojatool.ToolEval, map[string]string{"code": code})
	require.NoErrorf(t, err, "goja_eval(%q)", code)
	return decodeGojaResult(t, out)
}

// decodeGojaResult normalises whatever the chain hands back into the Result the
// tool returned plus its decoded Value.
func decodeGojaResult(t *testing.T, out any) (*gojatool.Result, any) {
	t.Helper()
	res, ok := out.(*gojatool.Result)
	if !ok {
		raw, err := json.Marshal(out)
		require.NoErrorf(t, err, "result is %T and does not marshal", out)
		res = &gojatool.Result{}
		require.NoErrorf(t, json.Unmarshal(raw, res), "result is %T: %s", out, raw)
	}
	var value any
	require.NoError(t, json.Unmarshal(res.Value, &value))
	return res, value
}

// exampleScripts is the set every test that is not about loading itself uses.
func exampleScripts() map[string]string {
	return map[string]string{
		"stats_summary.js":   scriptStatsSummary,
		"file_outline.js":    scriptFileOutline,
		"note_append.js":     scriptNoteAppend,
		"recursion_probe.js": scriptRecursionProbe,
		"slow_loop.js":       scriptSlowLoop,
		"denied_probe.js":    scriptDeniedRead,
	}
}

// ---------------------------------------------------------------------------
// Row 1 — registration: the provider exists, and it gates the way it was seeded
// ---------------------------------------------------------------------------

// TestSystem_Goja_RegisteredAndGatedUnderShippedPolicies asserts goja_eval is reachable through a real engine, computes correctly, and raises exactly one approval card naming itself per the seeded rule.
func TestSystem_Goja_RegisteredAndGatedUnderShippedPolicies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil) // approve everything

	for _, policy := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		t.Run(policy, func(t *testing.T) {
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()
			h.asker.reset()

			// The compute a model is bad at and a sandbox is trivially good at.
			_, value := h.eval(t, ctx, `
				function fib(n) { let a = 0, b = 1; for (let i = 0; i < n; i++) { [a, b] = [b, a + b]; } return a; }
				let squares = 0; for (let i = 1; i <= 100; i++) squares += i * i;
				({ fib20: fib(20), sumSquares: squares })
			`)
			m, ok := value.(map[string]any)
			require.Truef(t, ok, "value is %T", value)
			require.Equal(t, float64(6765), m["fib20"], "the 20th Fibonacci number")
			require.Equal(t, float64(338350), m["sumSquares"], "sum of squares 1..100")

			require.Equal(t, []string{"goja.goja_eval=allow"}, h.sink.decisions(),
				"%s: goja_eval must be EVALUATED by the seeded rule exactly once, and allowed. No decision at all would mean the provider is not addressed by any rule", policy)
			require.Emptyf(t, h.asker.seen(),
				"%s: goja_eval raised an approval card. It has no ambient I/O by construction, so the card protects nothing and costs an operator more than the call does — see the rule's own comment for what would revoke the allow", policy)
		})
	}
}

// TestSystem_Goja_ScriptToolsAreRegisteredAndFallToDefaultAction asserts a script tool carries no rule and so lands on default_action.
func TestSystem_Goja_ScriptToolsAreRegisteredAndFallToDefaultAction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil)
	ctx := context.Background()

	// The provider is registered on the engine, beside the built-in toolsets.
	require.Contains(t, h.engine.LocalTools, gojatool.ToolsProviderName)

	h.sink.drain()
	h.asker.reset()
	out, err := h.call(ctx, "stats_summary", map[string]string{"numbers": "3, 1, 4, 1, 5, 9, 2, 6"})
	require.NoError(t, err)
	_, value := decodeGojaResult(t, out)
	m, ok := value.(map[string]any)
	require.Truef(t, ok, "value is %T", value)
	require.Equal(t, float64(8), m["count"])
	require.Equal(t, float64(31), m["sum"])
	require.Equal(t, float64(3.5), m["median"])

	require.Equal(t, []string{"goja.stats_summary=approve"}, h.sink.decisions(),
		"a script tool must fall to default_action (approve), not to some rule meant for goja_eval")
	require.Equal(t, []string{"goja.stats_summary"}, h.asker.seen())
}

// ---------------------------------------------------------------------------
// Row 2 — the one boundary rule
// ---------------------------------------------------------------------------

// TestSystem_Goja_HostToolMeetsTheSameEnvelopeAModelWould asserts an allow-tier inner call (local_fs.read_file) produces a decision event with no card, and an approve-tier inner call (local_fs.write_file) raises a card naming that tool — approving the script does not approve what it reaches.
func TestSystem_Goja_HostToolMeetsTheSameEnvelopeAModelWould(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil) // approve everything
	ctx := context.Background()

	t.Run("an allow-tier inner call is evaluated and not gated", func(t *testing.T) {
		h.sink.drain()
		h.asker.reset()

		out, err := h.call(ctx, "file_outline", map[string]string{"path": "README.md"})
		require.NoError(t, err)
		_, value := decodeGojaResult(t, out)
		m, ok := value.(map[string]any)
		require.Truef(t, ok, "value is %T: %#v", value, value)
		require.Equal(t, "README.md", m["path"])
		require.Greater(t, m["lines"], float64(1), "the script did not actually read the file")
		marks, ok := m["marks"].([]any)
		require.True(t, ok)
		require.Len(t, marks, 2, "README.md has two Markdown headings")

		decisions := h.sink.decisions()
		require.Containsf(t, decisions, "local_fs.read_file=allow",
			"decisions were %v: the script's INNER call was never evaluated by the envelope — host.tool is wired to a repo that is not the gated one. This is the policy bypass the one boundary rule exists to prevent", decisions)
		require.Contains(t, decisions, "goja.file_outline=approve", "the script itself is still gated")
		require.Equal(t, []string{"goja.file_outline"}, h.asker.seen(),
			"an allow-tier inner call must not raise a card")
	})

	t.Run("an approve-tier inner call raises a card naming that tool", func(t *testing.T) {
		h.sink.drain()
		h.asker.reset()

		out, err := h.call(ctx, "note_append", map[string]string{"path": "notes.md", "text": "written by a script tool"})
		require.NoError(t, err)
		_, value := decodeGojaResult(t, out)
		m, ok := value.(map[string]any)
		require.Truef(t, ok, "value is %T: %#v", value, value)
		require.Equal(t, "notes.md", m["wrote"])

		decisions := h.sink.decisions()
		require.Containsf(t, decisions, "local_fs.write_file=approve",
			"decisions were %v: the script's write was not gated: approving a SCRIPT must not be a blanket approval of everything it reaches", decisions)
		require.Equal(t, []string{"goja.note_append", "local_fs.write_file"}, h.asker.seen(),
			"two cards, in order: the script, then the tool it reached")

		// And the write actually happened, through the real tool.
		body, rerr := os.ReadFile(filepath.Join(h.root, "notes.md"))
		require.NoError(t, rerr)
		require.Contains(t, string(body), "written by a script tool")
	})
}

// TestSystem_Goja_AnApprovalCardDoesNotBurnTheScriptsDeadline asserts the sandbox deadline does not run while host.tool is parked waiting on a human's approval answer.
func TestSystem_Goja_AnApprovalCardDoesNotBurnTheScriptsDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	humanLatency := 3 * gojatool.DefaultDeadline
	h := newGojaHarness(t, exampleScripts(), func(req hitlservice.ApprovalRequest) bool {
		if req.ToolsName == "local_fs" {
			time.Sleep(humanLatency)
		}
		return true
	})
	ctx := context.Background()
	h.sink.drain()
	h.asker.reset()

	start := time.Now()
	out, err := h.call(ctx, "note_append", map[string]string{"path": "slow.md", "text": "answered by a slow human"})
	elapsed := time.Since(start)

	require.NoErrorf(t, err,
		"a script died while an operator spent %s on its approval card (turn took %s). The deadline is a bound on COMPUTE; it must stop while host.tool is parked on a human",
		humanLatency, elapsed)
	_, value := decodeGojaResult(t, out)
	m, ok := value.(map[string]any)
	require.Truef(t, ok, "value is %T", value)
	require.Equal(t, "slow.md", m["wrote"])

	require.Greaterf(t, elapsed, humanLatency, "the slow asker never ran; the test proves nothing")
	require.Equal(t, []string{"goja.note_append", "local_fs.write_file"}, h.asker.seen())

	body, rerr := os.ReadFile(filepath.Join(h.root, "slow.md"))
	require.NoError(t, rerr)
	require.Contains(t, string(body), "answered by a slow human")
}

// TestSystem_Goja_DeniedInnerCallDoesNotBecomeData asserts a denied inner host.tool call surfaces to the script as a thrown JS exception, not as a value it can silently use as data.
func TestSystem_Goja_DeniedInnerCallDoesNotBecomeData(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	// Deny the inner write; approve the script itself so the run reaches it.
	h := newGojaHarness(t, exampleScripts(), func(req hitlservice.ApprovalRequest) bool {
		return req.ToolsName != "local_fs"
	})
	ctx := context.Background()
	h.sink.drain()
	h.asker.reset()

	out, err := h.call(ctx, "denied_probe", map[string]string{"path": "denied.md"})

	require.Contains(t, h.sink.decisions(), "local_fs.write_file=approve", "the gate ran")
	require.Equal(t, []string{"goja.denied_probe", "local_fs.write_file"}, h.asker.seen())

	require.Errorf(t, err, "a DENIED inner call came back as a value the script used as data: %#v", out)
	msg := err.Error()
	require.Containsf(t, msg, "denied", "the error does not say the call was denied: %q", msg)
	require.Containsf(t, msg, "recoverable", "the refusal carries no severity marker: %q", msg)

	// Nothing was written.
	_, statErr := os.Stat(filepath.Join(h.root, "denied.md"))
	require.True(t, os.IsNotExist(statErr), "a denied write created the file anyway")
}

// TestSystem_Goja_ScriptToolsStillGateWhileGojaEvalDoesNot asserts allowing goja_eval does not allow other tools on the same provider; a script tool still lands on default_action.
func TestSystem_Goja_ScriptToolsStillGateWhileGojaEvalDoesNot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil)
	for _, policy := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		t.Run(policy, func(t *testing.T) {
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()
			h.asker.reset()

			_, err := h.call(ctx, "stats_summary", map[string]string{"numbers": "1, 2, 3"})
			require.NoError(t, err)
			require.Equal(t, []string{"goja.stats_summary=approve"}, h.sink.decisions(),
				"%s: a script tool must still fall to default_action — the goja_eval allow is a rule about ONE tool, not about the provider", policy)
			require.Equal(t, []string{"goja.stats_summary"}, h.asker.seen())
		})
	}
}

// TestUnit_Goja_EvalTierMatchesInEveryPolicySource asserts goja_eval's allow tier is set in both shipped presets, and that the no-file fallback carries no such tier and asks instead.
func TestUnit_Goja_EvalTierMatchesInEveryPolicySource(t *testing.T) {
	ctx := context.Background()
	args := map[string]any{"code": "1 + 1"}

	fresh := t.TempDir()
	require.NoError(t, writeEmbeddedHITLPolicies(fresh, false))
	for _, name := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(fresh), testTenant, nopKV{}, libtracker.NoopTracker{}, name)
		r, err := svc.Evaluate(ctx, gojatool.ToolsProviderName, gojatool.ToolEval, args)
		require.NoError(t, err)
		require.Equalf(t, hitlservice.ActionAllow, r.Action,
			"%s must allow goja_eval: no filesystem, no network, no require, no process — an approval there protects nothing and every host.tool call it makes is gated on its own rule", name)
	}

	// No policy file anywhere: fail-closed, no allow tier, goja_eval asks like
	// everything else. The allow rule above exists only in seeded files.
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err := svc.Evaluate(ctx, gojatool.ToolsProviderName, gojatool.ToolEval, args)
	require.NoError(t, err)
	require.Equal(t, hitlservice.ActionApprove, r.Action, "the no-file fallback must ask — allow tiers live only in seeded, readable policy files")

	// A script tool must match nothing, in every source: unreviewed
	// operator-authored code falls to default_action by design.
	for _, name := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(fresh), testTenant, nopKV{}, libtracker.NoopTracker{}, name)
		r, err := svc.Evaluate(ctx, gojatool.ToolsProviderName, "some_operator_script", args)
		require.NoError(t, err)
		require.Equalf(t, hitlservice.ActionApprove, r.Action,
			"%s: a script tool must land on default_action, not inherit the goja_eval allow", name)
	}
}

// TestSystem_Goja_ADeniedCallIsCatchableByAScriptThatMeantIt asserts a script that wraps host.tool in try/catch can still catch a denial and keep its turn.
func TestSystem_Goja_ADeniedCallIsCatchableByAScriptThatMeantIt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), func(req hitlservice.ApprovalRequest) bool {
		return req.ToolsName != "local_fs"
	})
	ctx := context.Background()
	h.sink.drain()

	_, value := h.eval(t, ctx, `
		let outcome;
		try {
			host.tool("local_fs.write_file", { path: "nope.md", content: "x" });
			outcome = "wrote";
		} catch (e) {
			outcome = "refused: " + e.message.slice(0, 40);
		}
		({ outcome: outcome })
	`)
	m, ok := value.(map[string]any)
	require.Truef(t, ok, "value is %T", value)
	require.Contains(t, m["outcome"], "refused", "a caught denial must reach the script's own handler")
}

// ---------------------------------------------------------------------------
// Row 3 — the refusals, through the whole stack
// ---------------------------------------------------------------------------

// TestSystem_Goja_RefusalsHoldOnTheEnginePath asserts each documented sandbox limit, run through real dispatch, surfaces as a readable error rather than a panic, a hang, or a chain abort.
func TestSystem_Goja_RefusalsHoldOnTheEnginePath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil)
	ctx := context.Background()

	type refusal struct {
		name     string
		tool     string
		args     map[string]string
		contains []string
	}
	for _, tc := range []refusal{
		{
			name:     "a script may not call back into goja",
			tool:     "recursion_probe",
			args:     map[string]string{},
			contains: []string{"recursive goja tool call refused", "depth is exactly one"},
		},
		{
			name:     "a slow script hits its declared deadline",
			tool:     "slow_loop",
			args:     map[string]string{},
			contains: []string{"deadline", "no value was produced"},
		},
		{
			name:     "goja_eval hits the per-call deadline",
			tool:     gojatool.ToolEval,
			args:     map[string]string{"code": "while (true) {}", "deadline_ms": "100"},
			contains: []string{"deadline", "Do less work per call"},
		},
		{
			name:     "a Promise is refused, not marshaled as {}",
			tool:     gojatool.ToolEval,
			args:     map[string]string{"code": "Promise.resolve(42)"},
			contains: []string{"Promise", "no event loop"},
		},
		{
			name:     "unbounded recursion is contained",
			tool:     gojatool.ToolEval,
			args:     map[string]string{"code": "function f(){ return f(); } f()"},
			contains: []string{"maximum call depth", "Rewrite the recursion as a loop"},
		},
		{
			name:     "a syntax error names the line",
			tool:     gojatool.ToolEval,
			args:     map[string]string{"code": "function ( {"},
			contains: []string{"did not parse"},
		},
		{
			name:     "an unknown argument is refused by name",
			tool:     gojatool.ToolEval,
			args:     map[string]string{"code": "1", "timeout": "5"},
			contains: []string{"unknown argument", "timeout"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.sink.drain()
			start := time.Now()
			out, err := h.call(ctx, tc.tool, tc.args)
			elapsed := time.Since(start)

			require.Errorf(t, err, "the limit did not bite; got %#v", out)
			msg := err.Error()
			for _, want := range tc.contains {
				require.Containsf(t, msg, want, "the refusal does not teach %q: %s", want, msg)
			}
			require.Lessf(t, len(msg), 8192, "the refusal is %d bytes — script-controlled text is being echoed unbounded", len(msg))
			require.Containsf(t, msg, "recoverable", "the refusal carries no severity marker: %s", msg)
			require.Lessf(t, elapsed, 30*time.Second, "the refusal took %v — a limit that hangs is not a limit", elapsed)
		})
	}

	// The output cap truncates rather than errors, with an explicit marker.
	t.Run("the output cap truncates with a notice", func(t *testing.T) {
		h.sink.drain()
		res, _ := h.eval(t, ctx, `"x".repeat(200000)`)
		require.True(t, res.Truncated, "a 200 KB result was not capped")
		require.Contains(t, res.Notice, "output cap")
		require.Contains(t, res.Notice, "Filter or aggregate inside the script")
		require.LessOrEqual(t, len(res.Value), gojatool.DefaultOutputCap+len(res.Notice)+16)
	})

	// console.log is captured, not ambient, so a model's reflexive use of it
	// doesn't hit a ReferenceError.
	t.Run("console output comes back with the result", func(t *testing.T) {
		h.sink.drain()
		res, value := h.eval(t, ctx, `console.log("step", 1); console.warn("careful"); ({ ok: true })`)
		require.Equal(t, map[string]any{"ok": true}, value)
		require.Equal(t, []string{"step 1", "warn: careful"}, res.Logs)
	})
}

// ---------------------------------------------------------------------------
// Row 4 — startup and teardown
// ---------------------------------------------------------------------------

// TestSystem_Goja_ABadScriptIsAStartupErrorNamingTheFile asserts BuildEngine fails startup and names the file for a bad script, rather than silently skipping it.
func TestSystem_Goja_ABadScriptIsAStartupErrorNamingTheFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	for _, tc := range []struct {
		name     string
		file     string
		body     string
		contains []string
	}{
		{"a syntax error", "broken.js", "const tool = { name: ", []string{"broken.js", "did not parse"}},
		{"no tool descriptor", "nodesc.js", "function run() { return 1; }", []string{"nodesc.js", "does not export a `tool` object"}},
		{"no run function", "norun.js", `const tool = { name: "x", description: "d", schema: { type: "object", properties: {} } };`, []string{"norun.js", "does not define `function run(args)`"}},
		{"a name collision with goja_eval", "collide.js", `const tool = { name: "goja_eval", description: "d", schema: { type: "object", properties: {} } }; function run() { return 1; }`, []string{"collide.js", "reserved"}},
		{"a required argument that was never declared", "badschema.js", `const tool = { name: "x", description: "d", schema: { type: "object", properties: {}, required: ["missing"] } }; function run() { return 1; }`, []string{"badschema.js", "not in properties"}},
		{"a script that loops at load time", "loopy.js", `while (true) {}`, []string{"loopy.js", "deadline"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contenoxDir := filepath.Join(t.TempDir(), ".contenox")
			require.NoError(t, writeEmbeddedHITLPolicies(contenoxDir, true))
			scriptDir := filepath.Join(contenoxDir, "tools")
			require.NoError(t, os.MkdirAll(scriptDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(scriptDir, tc.file), []byte(tc.body), 0o644))

			ctx := context.Background()
			db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "goja-fail.db"), runtimetypes.SchemaSQLite)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			engine, err := BuildEngine(ctx, db, chatOpts{
				EffectiveDefaultModel:     "goja-e2e-fake-model",
				EffectiveDefaultProvider:  "ollama",
				EffectiveContext:          4096,
				EffectiveSkipBackendCycle: true,
				ContenoxDir:               contenoxDir,
			})
			if engine != nil && engine.Stop != nil {
				engine.Stop()
			}
			require.Error(t, err, "a broken script started the engine anyway — the operator's tool is missing and nothing said so")
			for _, want := range tc.contains {
				require.Containsf(t, err.Error(), want, "the startup error does not name %q: %v", want, err)
			}
		})
	}
}

// TestSystem_Goja_NoScriptDirIsNotAnError asserts a missing scripts directory is not a startup error, and goja_eval is still reachable.
func TestSystem_Goja_NoScriptDirIsNotAnError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, nil, nil)
	require.NoError(t, os.RemoveAll(h.scriptDir))

	_, value := h.eval(t, hitlservice.WithPolicyName(context.Background(), "hitl-policy-default.json"), `2 + 2`)
	require.Equal(t, float64(4), value)
}

// TestSystem_Goja_EngineStopClosesTheSandbox asserts a goja call arriving after engine.Stop is refused promptly and typed, not served or hung.
func TestSystem_Goja_EngineStopClosesTheSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil)
	ctx := context.Background()

	_, value := h.eval(t, ctx, `1 + 1`)
	require.Equal(t, float64(2), value)

	stopped := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.stop()
		stopped <- time.Since(start)
	}()
	select {
	case took := <-stopped:
		t.Logf("engine.Stop returned in %v", took)
	case <-time.After(30 * time.Second):
		t.Fatal("engine.Stop did not return within 30s — teardown is not bounded")
	}

	late := make(chan error, 1)
	go func() {
		_, err := h.call(ctx, gojatool.ToolEval, map[string]string{"code": "1 + 1"})
		late <- err
	}()
	select {
	case err := <-late:
		require.Error(t, err, "a goja call after engine.Stop was SERVED")
		require.Containsf(t, err.Error(), "shut down", "the refusal does not say the sandbox is closed: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("a goja call after engine.Stop HUNG — a closed sandbox must refuse, not block")
	}
}

// TestSystem_Goja_ScriptsAreIsolatedFromEachOther asserts one runtime per execution: separate calls must not see each other's global state.
func TestSystem_Goja_ScriptsAreIsolatedFromEachOther(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goja engine e2e: builds a real engine")
	}

	h := newGojaHarness(t, exampleScripts(), nil)
	ctx := context.Background()

	_, first := h.eval(t, ctx, `globalThis.leaked = "from the first call"; "ok"`)
	require.Equal(t, "ok", first)

	_, second := h.eval(t, ctx, `typeof globalThis.leaked`)
	require.Equal(t, "undefined", second, "state survived a call — the runtime is being reused")

	// And no ambient I/O exists to reach for in the first place.
	for _, probe := range []string{"require", "process", "fetch", "XMLHttpRequest", "setTimeout", "Deno"} {
		_, v := h.eval(t, ctx, fmt.Sprintf("typeof %s", probe))
		require.Equalf(t, "undefined", v, "%s is defined inside the sandbox", probe)
	}
}

// TestUnit_Goja_DenyMessageIsRecognisedByTheBridge asserts the bridge still recognises the gate's deny-string constant, so a denial can't silently turn back into data.
func TestUnit_Goja_DenyMessageIsRecognisedByTheBridge(t *testing.T) {
	require.True(t, gojatool.IsDenyMessage(localtools.DenyMessage),
		"the engine's deny message is no longer recognised by the bridge: a denied inner call would come back to a script as ordinary data")
	require.False(t, gojatool.IsDenyMessage("the file contains a sentence about users and denial"),
		"the deny detector is matching ordinary prose")
	require.False(t, gojatool.IsDenyMessage(""))
	_ = strings.TrimSpace("")
}
