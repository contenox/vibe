package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Engine-path harness
// ---------------------------------------------------------------------------

// goIntelAsker is the AskApproval an allow-tier toolset must NEVER reach. It
// records every ask and denies, so a policy regression shows up as a recorded
// ask plus a denied tool result rather than a hung prompt.
type goIntelAsker struct {
	mu   sync.Mutex
	asks []string
}

func (a *goIntelAsker) ask(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, req.ToolsName+"."+req.ToolName)
	return false, nil
}

func (a *goIntelAsker) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asks...)
}

// goIntelSink captures the engine's hitl_decision events: the ONLY honest proof
// of which envelope evaluated a call and what it decided.
type goIntelSink struct {
	mu     sync.Mutex
	events []taskengine.TaskEvent
}

func (s *goIntelSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *goIntelSink) Wants(kind taskengine.TaskEventKind) bool {
	return kind == taskengine.TaskEventHITLDecision || kind == taskengine.TaskEventApprovalRequested
}

func (s *goIntelSink) decisions() []taskengine.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]taskengine.TaskEvent, 0, len(s.events))
	for _, ev := range s.events {
		if ev.Kind == taskengine.TaskEventHITLDecision {
			out = append(out, ev)
		}
	}
	return out
}

func (s *goIntelSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
}

// repoRoot walks up from the test's working directory to the module root of
// THIS repository — the workspace gointel indexes in these tests.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "no go.mod above %s", dir)
		dir = parent
	}
}

type goIntelHarness struct {
	engine *Engine
	asker  *goIntelAsker
	sink   *goIntelSink
	root   string
}

// newGoIntelHarness builds a REAL engine through BuildEngine with HITL on and
// the SHIPPED policy presets seeded into an isolated .contenox dir. HOME is
// redirected so hitlPolicySource's ~/.contenox fallback cannot reach the
// developer's own (possibly edited) envelopes.
func newGoIntelHarness(t *testing.T, allowedDir string) *goIntelHarness {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, writeEmbeddedHITLPolicies(contenoxDir, true))

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "gointel-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	asker := &goIntelAsker{}
	sink := &goIntelSink{}

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
	t.Cleanup(engine.Stop)

	return &goIntelHarness{engine: engine, asker: asker, sink: sink, root: allowedDir}
}

// call runs one gointel tool through the engine's chain executor — the real
// dispatch path, HITL wrapper included.
func (h *goIntelHarness) call(ctx context.Context, tool string, args map[string]string) (any, error) {
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

func TestSystem_GoIntel_EnginePath_Smoke(t *testing.T) {
	h := newGoIntelHarness(t, repoRoot(t))
	ctx := context.Background()

	start := time.Now()
	out, err := h.call(ctx, gointel.ToolDefinition, map[string]string{"symbol": "frame.StyleBrand"})
	t.Logf("go_definition (cold) took %v", time.Since(start))
	require.NoError(t, err)
	t.Logf("out = %#v", out)
	t.Logf("decisions = %#v", h.sink.decisions())
	require.Empty(t, h.asker.seen())
}
