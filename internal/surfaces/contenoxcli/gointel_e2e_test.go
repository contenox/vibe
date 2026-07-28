package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// gointel on the engine path: asserts a gointel call travelling through
// BuildEngine, the aggregate tools repo, the real HITL wrapper, and the
// shipped policy file comes back with a fact rather than an approval prompt.
// An ask or a deny is a failure; only allow passes.
// ---------------------------------------------------------------------------

// gointelAsker is the approval callback an allow-tier toolset must never reach.
type gointelAsker struct {
	mu   sync.Mutex
	asks []string
}

func (a *gointelAsker) ask(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, req.ToolsName+"."+req.ToolName)
	return false, nil
}

func (a *gointelAsker) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asks...)
}

// gointelSink captures the engine's hitl_decision events: the only evidence
// of which envelope evaluated a call, since a green result alone cannot tell
// an allow from a policy that was never consulted.
type gointelSink struct {
	mu     sync.Mutex
	events []taskengine.TaskEvent
}

func (s *gointelSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *gointelSink) Wants(kind taskengine.TaskEventKind) bool {
	return kind == taskengine.TaskEventHITLDecision || kind == taskengine.TaskEventApprovalRequested
}

func (s *gointelSink) drain() []taskengine.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events = nil
	return out
}

// gointelRepoRoot walks up from the test's working directory to this
// repository's module root — the workspace these tests index.
func gointelRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqualf(t, parent, dir, "no go.mod above %s", dir)
		dir = parent
	}
}

type gointelHarness struct {
	engine      *Engine
	asker       *gointelAsker
	sink        *gointelSink
	root        string
	contenoxDir string
}

// newGointelHarness builds a real engine through BuildEngine with HITL on and
// the shipped policy presets seeded into an isolated .contenox dir, so these
// tests evaluate the envelopes this binary ships, not whatever is on the
// machine.
func newGointelHarness(t *testing.T, allowedDir string) *gointelHarness {
	t.Helper()

	// No interactive answer is obtainable from this process: a prompt that
	// tries to read one fails at once rather than parking the test.
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

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "gointel-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	asker := &gointelAsker{}
	sink := &gointelSink{}

	engine, err := BuildEngine(ctx, db, chatOpts{
		EffectiveDefaultModel:        "gointel-e2e-fake-model",
		EffectiveDefaultProvider:     "ollama",
		EffectiveContext:             4096,
		EffectiveHITL:                true,
		EffectiveLocalExecAllowedDir: allowedDir,
		EffectiveSkipBackendCycle:    true,
		EffectiveAskApproval:         asker.ask,
		EffectiveTaskEventSink:       sink,
		ContenoxDir:                  contenoxDir,
	})
	require.NoError(t, err)

	h := &gointelHarness{engine: engine, asker: asker, sink: sink, root: allowedDir, contenoxDir: contenoxDir}
	t.Cleanup(h.stop)
	return h
}

func (h *gointelHarness) stop() {
	if h.engine != nil && h.engine.Stop != nil {
		h.engine.Stop()
		h.engine.Stop = nil
	}
}

// call runs one gointel tool through the engine's chain executor — the real
// dispatch, HITL wrapper and shipped policy included.
func (h *gointelHarness) call(ctx context.Context, tool string, args map[string]string) (any, error) {
	chain := &taskengine.TaskChainDefinition{
		ID:          "gointel-e2e",
		Description: "one gointel tool call through the real engine",
		Tasks: []taskengine.TaskDefinition{{
			ID:      "call",
			Handler: taskengine.HandleTools,
			Tools: &taskengine.ToolsCall{
				Name:     gointel.ToolsProviderName,
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

// requireAllowedWithoutAsking is the headline assertion, applied to every call.
func (h *gointelHarness) requireAllowedWithoutAsking(t *testing.T, policyName, tool string) {
	t.Helper()
	events := h.sink.drain()

	var decisions []taskengine.TaskEvent
	for _, ev := range events {
		switch ev.Kind {
		case taskengine.TaskEventHITLDecision:
			if ev.HookName == gointel.ToolsProviderName {
				decisions = append(decisions, ev)
			}
		case taskengine.TaskEventApprovalRequested:
			t.Errorf("%s: %s.%s RAISED AN APPROVAL — a read toolset that interrupts is a read toolset nobody enables",
				policyName, ev.HookName, ev.ToolName)
		}
	}

	require.Lenf(t, decisions, 1, "%s: expected exactly one HITL decision for %s, got %d (the gate was skipped or ran twice)",
		policyName, tool, len(decisions))
	d := decisions[0]
	require.Equalf(t, tool, d.ToolName, "%s: decision was recorded for the wrong tool", policyName)
	require.Equalf(t, string(hitlservice.ActionAllow), d.HITLAction,
		"%s: gointel.%s was %q, not allow — every gointel tool is a pure read and must never gate", policyName, tool, d.HITLAction)
	require.Equalf(t, policyName, d.HITLPolicyName, "%s: a different envelope evaluated the call", policyName)
	if d.HITLApprovalRequested != nil {
		require.Falsef(t, *d.HITLApprovalRequested, "%s: gointel.%s requested an approval", policyName, tool)
	}
	require.Emptyf(t, h.asker.seen(), "%s: the approval callback was invoked for %s", policyName, tool)
}

// ---------------------------------------------------------------------------
// Row 1 — the headline: real engine, real wrapper, shipped policies
// ---------------------------------------------------------------------------

// TestSystem_GoIntel_EnginePathAnswersWithoutAskingUnderShippedPolicies asserts three gointel tools, run against this repository through a real engine under each shipped policy, return verifiable ground truth with an allow decision.
func TestSystem_GoIntel_EnginePathAnswersWithoutAskingUnderShippedPolicies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	root := gointelRepoRoot(t)
	h := newGointelHarness(t, root)

	for _, policy := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			// Pinning the policy here means the decision events below name the
			// file that was actually consulted.
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()

			t.Run("go_definition names the real declaration", func(t *testing.T) {
				start := time.Now()
				out, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand"})
				require.NoError(t, err)
				h.requireAllowedWithoutAsking(t, policy, gointel.ToolDefinition)

				res, ok := out.(*gointel.DefinitionResult)
				require.Truef(t, ok, "result is %T, not the declared schema", out)
				require.Equal(t, "github.com/contenox/beam/internal/surfaces/beamtui/frame.StyleBrand", res.Symbol)
				require.Equal(t, "const", res.Kind)
				require.Equal(t, "internal/surfaces/beamtui/frame/frame.go:37:2", res.Location,
					"ground truth moved: check where frame.StyleBrand is declared")
				require.Contains(t, res.Line, "StyleBrand")
				require.Equal(t, "github.com/contenox/beam", res.Module)
				require.Contains(t, res.Toolchain, "advisory")
				t.Logf("go_definition took %v", time.Since(start))
			})

			t.Run("go_describe carries hover-grade truth", func(t *testing.T) {
				start := time.Now()
				out, err := h.call(ctx, gointel.ToolDescribe, map[string]string{"symbol": "frame.StyleID"})
				require.NoError(t, err)
				h.requireAllowedWithoutAsking(t, policy, gointel.ToolDescribe)

				res, ok := out.(*gointel.DescribeResult)
				require.Truef(t, ok, "result is %T", out)
				require.Equal(t, "github.com/contenox/beam/internal/surfaces/beamtui/frame.StyleID", res.Symbol)
				require.Equal(t, "type", res.Kind)
				require.Equal(t, "string", res.Underlying, "StyleID is a defined string type")
				require.Contains(t, res.Location, "internal/surfaces/beamtui/frame/frame.go:")
				require.NotEmpty(t, res.Doc, "the declaration's doc comment is the point of describe")
				t.Logf("go_describe took %v", time.Since(start))
			})

			t.Run("go_references finds the real call sites", func(t *testing.T) {
				start := time.Now()
				out, err := h.call(ctx, gointel.ToolReferences, map[string]string{"symbol": "frame.StyleBrand", "max": "200"})
				require.NoError(t, err)
				h.requireAllowedWithoutAsking(t, policy, gointel.ToolReferences)

				res, ok := out.(*gointel.ReferencesResult)
				require.Truef(t, ok, "result is %T", out)
				require.Greaterf(t, res.Total, 10, "frame.StyleBrand is used across the TUI; %d locations means resolution collapsed", res.Total)
				require.Greater(t, len(res.Files), 3, "the uses span several files")
				for _, f := range res.Files {
					require.Falsef(t, filepath.IsAbs(f.File), "%s is absolute; anchors are workspace-relative so they can be passed to a read tool", f.File)
					require.NotContainsf(t, f.File, "..", "%s escapes the workspace", f.File)
					require.False(t, strings.HasSuffix(f.File, "_test.go"),
						"tests are excluded from the build context, so %s must not appear", f.File)
				}
				t.Logf("go_references found %d locations in %d files in %v", res.Total, len(res.Files), time.Since(start))
			})
		})
	}
}

// TestSystem_GoIntel_EnginePathHonoursTheEnvelopeOnDisk asserts pinning hitl-policy-acpx.json (a deny floor, no gointel rule) denies the identical call that hitl-policy-default.json allows, proving the decision comes from the file on disk.
func TestSystem_GoIntel_EnginePathHonoursTheEnvelopeOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	h := newGointelHarness(t, gointelRepoRoot(t))
	ctx := hitlservice.WithPolicyName(context.Background(), "hitl-policy-acpx.json")
	h.sink.drain()

	out, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand"})
	require.NoError(t, err, "a denied tool returns the deny MESSAGE, not a chain error")

	var decision *taskengine.TaskEvent
	for _, ev := range h.sink.drain() {
		if ev.Kind == taskengine.TaskEventHITLDecision && ev.HookName == gointel.ToolsProviderName {
			ev := ev
			decision = &ev
		}
	}
	require.NotNil(t, decision, "no HITL decision was recorded")
	require.Equal(t, "hitl-policy-acpx.json", decision.HITLPolicyName)
	require.Equalf(t, string(hitlservice.ActionDeny), decision.HITLAction,
		"acpx has a deny floor and no gointel rule, so the identical call must be denied — if it is allowed, the pinned FILE is not what decided")
	require.IsType(t, "", out, "a denied call returns the deny message")

	// Nothing was asked: acpx is allow/deny only, there is no operator to prompt.
	require.Empty(t, h.asker.seen())
}

// ---------------------------------------------------------------------------
// Row 2 — hostile arguments through the engine's own dispatch
// ---------------------------------------------------------------------------

// TestSystem_GoIntel_EnginePathRefusesHostileArgumentsWithoutEscaping asserts hostile arguments, through the engine's own string-coerced argument marshalling, refuse as a task error rather than panic or escape containment.
func TestSystem_GoIntel_EnginePathRefusesHostileArgumentsWithoutEscaping(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	root := gointelRepoRoot(t)
	h := newGointelHarness(t, root)
	ctx := context.Background()

	huge := strings.Repeat("Aa0", 3500) // ~10 KB

	cases := []struct {
		name string
		tool string
		args map[string]string
		// wantContained marks the cases whose refusal must name containment.
		wantContained bool
	}{
		{"symbol traversal", gointel.ToolDefinition, map[string]string{"symbol": "../../etc/passwd"}, false},
		{"symbol absolute", gointel.ToolDefinition, map[string]string{"symbol": "/etc/passwd"}, false},
		{"symbol 10KB", gointel.ToolDescribe, map[string]string{"symbol": huge}, false},
		{"symbol bidi", gointel.ToolDefinition, map[string]string{"symbol": "‮StyleBrand‭"}, false},
		{"symbol empty", gointel.ToolDefinition, map[string]string{"symbol": ""}, false},
		{"symbol format verbs", gointel.ToolDefinition, map[string]string{"symbol": "%s%s%n"}, false},
		{"symbol nul byte", gointel.ToolDefinition, map[string]string{"symbol": "frame\x00.StyleBrand"}, false},

		{"dir traversal", gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand", "dir": "../../.."}, true},
		{"dir absolute outside", gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand", "dir": "/etc"}, true},
		{"dir 10KB", gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand", "dir": huge}, false},

		{"scope garbage", gointel.ToolDiagnostics, map[string]string{"scope": "everything"}, false},
		{"passes unknown", gointel.ToolDiagnostics, map[string]string{"scope": "all", "passes": "notapass"}, false},
		{"passes all plus unknown", gointel.ToolDiagnostics, map[string]string{"scope": "all", "passes": "all,notapass"}, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h.sink.drain()
			out, err := h.call(ctx, tc.tool, tc.args)
			// A hostile argument is the tool's problem to refuse, not the policy's to gate on.
			h.requireAllowedWithoutAsking(t, "hitl-policy-default.json", tc.tool)

			require.Errorf(t, err, "hostile input was accepted, returning %#v", out)
			msg := err.Error()
			require.Lessf(t, len(msg), 8192,
				"the error is %d bytes — a model-supplied argument is being echoed back unbounded", len(msg))
			require.NotContains(t, msg, "\x00", "a NUL byte reached the model's context unescaped")
			require.Truef(t,
				strings.Contains(msg, "recoverable: adjust parameters and retry") || strings.Contains(msg, "(fatal:"),
				"the refusal carries no severity marker: %q", msg)
			if tc.wantContained {
				require.Containsf(t, msg, "allowed directory",
					"an escape attempt must be refused BY NAME, so the model learns the boundary exists: %q", msg)
			}
			// Whatever the message says, it never names a path outside the workspace.
			require.NotContains(t, msg, "/etc/passwd:")
		})
	}
}

// TestSystem_GoIntel_EnginePathTolerantOfSloppyButHonestArguments asserts merely sloppy (not malicious) arguments — stringified numbers, redundant path segments — answer rather than refuse.
func TestSystem_GoIntel_EnginePathTolerantOfSloppyButHonestArguments(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	h := newGointelHarness(t, gointelRepoRoot(t))
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		tool string
		args map[string]string
	}{
		{"max as a string", gointel.ToolReferences, map[string]string{"symbol": "frame.StyleBrand", "max": "5"}},
		{"max above the ceiling", gointel.ToolReferences, map[string]string{"symbol": "frame.StyleBrand", "max": "1000000000"}},
		{"max negative", gointel.ToolReferences, map[string]string{"symbol": "frame.StyleBrand", "max": "-1"}},
		{"max zero", gointel.ToolReferences, map[string]string{"symbol": "frame.StyleBrand", "max": "0"}},
		{"symbol with surrounding space", gointel.ToolDefinition, map[string]string{"symbol": "  frame.StyleBrand  "}},
		{"full import path", gointel.ToolDefinition, map[string]string{"symbol": "github.com/contenox/beam/internal/surfaces/beamtui/frame.StyleBrand"}},
		{"import path suffix", gointel.ToolDefinition, map[string]string{"symbol": "beamtui/frame.StyleBrand"}},
		{"dir with redundant segments", gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand", "dir": "internal/../internal/surfaces"}},
		{"dir naming a file", gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand", "dir": "internal/surfaces/beamtui/frame/frame.go"}},
		{"passes comma separated", gointel.ToolDiagnostics, map[string]string{"scope": "package", "target": "gointel", "passes": "printf, unreachable"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h.sink.drain()
			_, err := h.call(ctx, tc.tool, tc.args)
			require.NoErrorf(t, err, "a sloppy but unambiguous argument was refused")
			h.requireAllowedWithoutAsking(t, "hitl-policy-default.json", tc.tool)
		})
	}

	// The cap is honoured, not merely tolerated.
	out, err := h.call(ctx, gointel.ToolReferences, map[string]string{"symbol": "frame.StyleBrand", "max": "5"})
	require.NoError(t, err)
	require.LessOrEqual(t, out.(*gointel.ReferencesResult).Shown, 5)
}

// ---------------------------------------------------------------------------
// Rows 3 and 4 — teardown and freshness, on the engine path
// ---------------------------------------------------------------------------

// TestSystem_GoIntel_EngineStopClosesTheIndexForLateCalls asserts a gointel call arriving after engine.Stop is refused promptly and typed, not served into a cache whose reaper has already exited.
func TestSystem_GoIntel_EngineStopClosesTheIndexForLateCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	h := newGointelHarness(t, gointelRepoRoot(t))
	ctx := context.Background()

	_, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand"})
	require.NoError(t, err, "warm-up")

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
		_, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand"})
		late <- err
	}()
	select {
	case err := <-late:
		require.Error(t, err, "a gointel call after engine.Stop was SERVED — it rebuilt a snapshot into a cache with no reaper")
		require.Truef(t, strings.Contains(err.Error(), "shut down"),
			"the refusal does not say the index is closed: %q", err)
	case <-time.After(30 * time.Second):
		t.Fatal("a gointel call after engine.Stop HUNG — a closed index must refuse, not block")
	}
}

// TestSystem_GoIntel_EnginePathConcurrentCallsStayCorrect asserts the engine path stays correct under concurrent calls from many goroutines while a writer churns a workspace file, catching an unlocked shared-snapshot read under -race.
func TestSystem_GoIntel_EnginePathConcurrentCallsStayCorrect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	// A throwaway module, not this repo: the writer edits source, and the only
	// acceptable place to do that is a directory the test owns.
	root := t.TempDir()
	writeGointelModule(t, root)
	h := newGointelHarness(t, root)
	ctx := context.Background()

	_, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "core.Widget"})
	require.NoError(t, err, "warm-up")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	calls := []struct {
		tool string
		args map[string]string
	}{
		{gointel.ToolDefinition, map[string]string{"symbol": "core.Widget"}},
		{gointel.ToolDescribe, map[string]string{"symbol": "core.Widget"}},
		{gointel.ToolSymbols, map[string]string{"target": "core"}},
		{gointel.ToolReferences, map[string]string{"symbol": "core.Widget", "max": "20"}},
		{gointel.ToolDiagnostics, map[string]string{"scope": "changed"}},
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < 8; n++ {
				select {
				case <-stop:
					return
				default:
				}
				c := calls[(seed+n)%len(calls)]
				if _, err := h.call(ctx, c.tool, c.args); err != nil {
					t.Errorf("concurrent %s: %v", c.tool, err)
					return
				}
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 20; n++ {
			select {
			case <-stop:
				return
			default:
			}
			body := fmt.Sprintf("package core\n\n// Widget is the fixture type.\ntype Widget struct{ N int }\n\n// Churn%d moves the file.\nfunc Churn%d() int { return 0 }\n", n, n)
			tmp := filepath.Join(root, "core", "core.go.tmp")
			require.NoError(t, os.WriteFile(tmp, []byte(body), 0o644))
			require.NoError(t, os.Rename(tmp, filepath.Join(root, "core", "core.go")))
			time.Sleep(10 * time.Millisecond)
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		close(stop)
		t.Fatal("concurrent engine-path calls did not drain within 120s — a deadlock between the rebuild lock and the query path")
	}

	// No call was ever gated, however many ran at once.
	require.Empty(t, h.asker.seen())
}

// TestSystem_GoIntel_EnginePathSeesAnEditImmediately asserts an edit, a new file, a deletion, and a new call site are all picked up by the mtime sweep, the only mechanism that keeps the index from going stale.
func TestSystem_GoIntel_EnginePathSeesAnEditImmediately(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine and type-checks this repository")
	}

	root := t.TempDir()
	writeGointelModule(t, root)
	h := newGointelHarness(t, root)
	ctx := context.Background()

	_, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "core.Widget"})
	require.NoError(t, err, "warm-up")
	_, err = h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "core.Added"})
	require.Error(t, err, "the symbol must not exist yet")

	corePath := filepath.Join(root, "core", "core.go")

	// (1) An edit to an existing file, written exactly as write_file writes one.
	src, err := os.ReadFile(corePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(corePath, append(src, []byte(
		"\n// Added arrived after the snapshot was built.\nfunc Added() int { return 7 }\n")...), 0o644))

	out, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "core.Added"})
	require.NoError(t, err, "the mtime sweep did not see an edit made moments earlier")
	require.Contains(t, out.(*gointel.DefinitionResult).Location, "core/core.go:")

	// (2) A brand-new file in an existing package — invisible to every per-file
	// stat, caught only by the package DIRECTORY's own stamp.
	require.NoError(t, os.WriteFile(filepath.Join(root, "core", "extra.go"),
		[]byte("package core\n\n// FromANewFile did not exist in any indexed file.\nfunc FromANewFile() int { return 1 }\n"), 0o644))
	_, err = h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "core.FromANewFile"})
	require.NoError(t, err, "a new file in an existing package was not seen")

	// go_diagnostics reports on what it watched change, unprompted.
	diag, err := h.call(ctx, gointel.ToolDiagnostics, map[string]string{"scope": "changed"})
	require.NoError(t, err)
	require.NotEmpty(t, diag.(*gointel.DiagnosticsResult).Packages,
		"scope=changed saw nothing after two writes: %q", diag.(*gointel.DiagnosticsResult).Note)

	// (3) A deletion. The dangerous direction: a confident file:line for a symbol
	// that no longer exists is worse than a refusal.
	require.NoError(t, os.Remove(filepath.Join(root, "core", "extra.go")))
	_, err = h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "core.FromANewFile"})
	require.Error(t, err, "a deleted symbol still resolved — the snapshot is stale in the one direction that lies")

	// (4) go_references reflects a call site added seconds ago.
	before, err := h.call(ctx, gointel.ToolReferences, map[string]string{"symbol": "core.Widget"})
	require.NoError(t, err)
	baseline := before.(*gointel.ReferencesResult).Uses

	usePath := filepath.Join(root, "use", "use.go")
	useSrc, err := os.ReadFile(usePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(usePath, append(useSrc, []byte(
		"\n// Another is a call site added after the snapshot was built.\nfunc Another() core.Widget { return core.Widget{} }\n")...), 0o644))

	after, err := h.call(ctx, gointel.ToolReferences, map[string]string{"symbol": "core.Widget"})
	require.NoError(t, err)
	// The added function mentions core.Widget twice (return type and composite
	// literal), and Uses counts raw identifier occurrences.
	require.Equalf(t, baseline+2, after.(*gointel.ReferencesResult).Uses,
		"a stale snapshot would still report %d uses", baseline)

	require.Empty(t, h.asker.seen())
}

// writeGointelModule materialises the small stdlib-only module the
// edit-and-teardown tests own and mutate.
func writeGointelModule(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"go.mod":       "module example.com/e2efixture\n\ngo 1.21\n",
		"core/core.go": "package core\n\n// Widget is the fixture type.\ntype Widget struct{ N int }\n",
		"use/use.go":   "package use\n\nimport \"example.com/e2efixture/core\"\n\n// Make builds a Widget.\nfunc Make() core.Widget { return core.Widget{} }\n",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
}

// TestSystem_GoIntel_EnginePathWithNoAllowedDirRefusesActionably asserts that with no allowed dir (the default `contenox beam` wiring), gointel's refusal is marked fatal and names how to supply the root, rather than reading as broken.
func TestSystem_GoIntel_EnginePathWithNoAllowedDirRefusesActionably(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine")
	}

	h := newGointelHarness(t, "") // exactly what `contenox beam` passes today

	_, err := h.call(context.Background(), gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand"})
	require.Error(t, err, "an index with no workspace root answered a query")
	msg := err.Error()
	require.Containsf(t, msg, "no workspace root", "the refusal does not name the cause: %q", msg)
	require.Containsf(t, msg, "(fatal:", "the refusal is not marked fatal, but no retry can fix a wiring gap: %q", msg)
	require.Containsf(t, msg, "--local-exec-allowed-dir", "the refusal does not name how to supply the root: %q", msg)

	// It still does not gate: an unusable tool that also interrupts would be the worst of both.
	h.requireAllowedWithoutAsking(t, "hitl-policy-default.json", gointel.ToolDefinition)
}

// TestSystem_GoIntel_EnginePathHandlesANonGoWorkspace asserts gointel, always registered even with no Go present, refuses with a typed teaching error and does not gate.
func TestSystem_GoIntel_EnginePathHandlesANonGoWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gointel engine e2e: builds a real engine")
	}

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# not a go project\n"), 0o644))
	h := newGointelHarness(t, root)

	_, err := h.call(context.Background(), gointel.ToolDefinition, map[string]string{"symbol": "Anything"})
	require.Error(t, err)
	require.True(t, errors.Is(err, gointel.ErrNoModule) || strings.Contains(err.Error(), "no Go module"),
		"a non-Go workspace must produce the typed no-module refusal, got: %v", err)
	require.Contains(t, err.Error(), "go.mod", "the refusal does not say what is missing")
	h.requireAllowedWithoutAsking(t, "hitl-policy-default.json", gointel.ToolDefinition)
}
