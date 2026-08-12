package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/searchtool"
	"github.com/contenox/contenox/internal/services/workspaceindex"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type wsearchAsker struct {
	mu   sync.Mutex
	asks []hitlservice.ApprovalRequest
}

func (a *wsearchAsker) ask(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, req)
	return false, nil
}

func (a *wsearchAsker) seen() []hitlservice.ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]hitlservice.ApprovalRequest(nil), a.asks...)
}

func (a *wsearchAsker) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = nil
}

type wsearchSink struct {
	mu     sync.Mutex
	events []taskengine.TaskEvent
}

func (s *wsearchSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *wsearchSink) Wants(kind taskengine.TaskEventKind) bool {
	return kind == taskengine.TaskEventHITLDecision || kind == taskengine.TaskEventApprovalRequested
}

func (s *wsearchSink) drain() []taskengine.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events = nil
	return out
}

type wsearchHarness struct {
	engine      *Engine
	asker       *wsearchAsker
	sink        *wsearchSink
	db          libdb.DBManager
	contenoxDir string
	workspaceID string
}

func newWSearchHarness(t *testing.T, prepare func(*testing.T, libdb.DBManager), opts ...func(*chatOpts)) *wsearchHarness {
	t.Helper()

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
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wsearch-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	if prepare != nil {
		prepare(t, db)
	}

	asker := &wsearchAsker{}
	sink := &wsearchSink{}

	co := chatOpts{
		EffectiveDefaultModel:     "wsearch-e2e-fake-model",
		EffectiveDefaultProvider:  "ollama",
		EffectiveContext:          4096,
		EffectiveHITL:             true,
		EffectiveSkipBackendCycle: true,
		EffectiveAskApproval:      asker.ask,
		EffectiveTaskEventSink:    sink,
		ContenoxDir:               contenoxDir,
	}
	for _, o := range opts {
		o(&co)
	}

	engine, err := BuildEngine(ctx, db, co)
	require.NoError(t, err)

	h := &wsearchHarness{
		engine:      engine,
		asker:       asker,
		sink:        sink,
		db:          db,
		contenoxDir: contenoxDir,
		workspaceID: ResolveWorkspaceID(contenoxDir),
	}
	t.Cleanup(h.stop)
	return h
}

func (h *wsearchHarness) stop() {
	if h.engine != nil && h.engine.Stop != nil {
		h.engine.Stop()
		h.engine.Stop = nil
	}
}

func (h *wsearchHarness) call(ctx context.Context, args map[string]string) (any, error) {
	chain := &taskengine.TaskChainDefinition{
		ID:          "workspace-search-e2e",
		Description: "one workspace_search call through the real engine",
		Tasks: []taskengine.TaskDefinition{{
			ID:      "call",
			Handler: taskengine.HandleTools,
			Tools: &taskengine.ToolsCall{
				Name:     searchtool.ToolsProviderName,
				ToolName: searchtool.ToolSearch,
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

func (h *wsearchHarness) decisionFor(t *testing.T, events []taskengine.TaskEvent) taskengine.TaskEvent {
	t.Helper()
	var decisions []taskengine.TaskEvent
	for _, ev := range events {
		if ev.Kind == taskengine.TaskEventHITLDecision && ev.HookName == searchtool.ToolsProviderName {
			decisions = append(decisions, ev)
		}
	}
	require.Lenf(t, decisions, 1, "expected exactly one HITL decision for %s, got %d — the gate was skipped or ran twice",
		searchtool.ToolSearch, len(decisions))
	return decisions[0]
}

func writeWSearchPolicy(t *testing.T, contenoxDir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, name), []byte(body), 0o644))
}

// TestSystem_WorkspaceSearch_EnginePathAllowedWithoutAskingUnderShippedPolicies asserts every shipped interactive envelope allows workspace_search unasked, and a missing index answers with a result naming `contenox index`, not an error.
func TestSystem_WorkspaceSearch_EnginePathAllowedWithoutAskingUnderShippedPolicies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)

	for _, policy := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()
			h.asker.reset()

			out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
			require.NoError(t, err)

			events := h.sink.drain()
			for _, ev := range events {
				require.NotEqualf(t, taskengine.TaskEventApprovalRequested, ev.Kind,
					"%s: %s.%s RAISED AN APPROVAL — a read toolset that interrupts is a read toolset nobody enables",
					policy, ev.HookName, ev.ToolName)
			}

			d := h.decisionFor(t, events)
			require.Equal(t, searchtool.ToolSearch, d.ToolName, "%s: the decision was recorded for the wrong tool", policy)
			require.Equalf(t, string(hitlservice.ActionAllow), d.HITLAction,
				"%s: workspace.%s was %q, not allow — it is a read of files the agent may already read", policy, searchtool.ToolSearch, d.HITLAction)
			require.Equalf(t, policy, d.HITLPolicyName, "%s: a different envelope evaluated the call", policy)
			if d.HITLApprovalRequested != nil {
				require.Falsef(t, *d.HITLApprovalRequested, "%s: workspace_search requested an approval", policy)
			}
			require.Emptyf(t, h.asker.seen(), "%s: the approval callback was invoked", policy)

			res, ok := out.(*searchtool.Result)
			require.Truef(t, ok, "%s: result is %T, not the tool's payload — the call may have been answered by the policy", policy, out)
			require.Empty(t, res.Hits)
			require.Containsf(t, res.Note, "contenox index",
				"%s: a workspace with no index must answer with the command that fixes it: %q", policy, res.Note)
		})
	}
}

// TestSystem_WorkspaceSearch_NoRuleFallsToDefaultActionAndAsks asserts an envelope with no workspace rule raises an approval rather than silently allowing.
func TestSystem_WorkspaceSearch_NoRuleFallsToDefaultActionAndAsks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)
	writeWSearchPolicy(t, h.contenoxDir, "hitl-policy-norule.json", `{
	  "default_action": "approve",
	  "rules": [
	    {"tools": "local_fs", "tool": "read_file", "action": "allow"}
	  ]
	}`)

	ctx := hitlservice.WithPolicyName(context.Background(), "hitl-policy-norule.json")
	h.sink.drain()
	h.asker.reset()

	out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
	require.NoError(t, err, "a call held at an approval and then denied returns the denial, not a chain error")

	events := h.sink.drain()
	d := h.decisionFor(t, events)
	require.Equal(t, "hitl-policy-norule.json", d.HITLPolicyName)
	require.Equalf(t, string(hitlservice.ActionApprove), d.HITLAction,
		"an envelope with no workspace rule must fall to default_action=approve, got %q — a toolset the policy never mentions must not be silently allowed", d.HITLAction)

	asks := h.asker.seen()
	require.Lenf(t, asks, 1, "default_action=approve did not reach the asker: %d ask(s)", len(asks))
	require.Equal(t, searchtool.ToolsProviderName, asks[0].ToolsName)
	require.Equal(t, searchtool.ToolSearch, asks[0].ToolName)

	// The decision event records that an approval was raised; the CLI's inline asker publishes no separate approval_requested event.
	require.NotNil(t, d.HITLApprovalRequested, "the decision does not record whether an approval was raised")
	require.True(t, *d.HITLApprovalRequested, "the decision says no approval was raised, but the call was held at one")

	require.IsTypef(t, "", out, "a refused call must return the denial message, got %T", out)
}

// TestSystem_WorkspaceSearch_ExplicitDenyRuleIsHonoured asserts an explicit deny rule denies the toolset without consulting the asker or failing the turn.
func TestSystem_WorkspaceSearch_ExplicitDenyRuleIsHonoured(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)
	writeWSearchPolicy(t, h.contenoxDir, "hitl-policy-denyws.json", `{
	  "default_action": "approve",
	  "rules": [
	    {"tools": "workspace", "action": "deny"}
	  ]
	}`)

	ctx := hitlservice.WithPolicyName(context.Background(), "hitl-policy-denyws.json")
	h.sink.drain()
	h.asker.reset()

	out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
	require.NoError(t, err, "a denied tool returns the deny MESSAGE, not a chain error that ends the turn")

	d := h.decisionFor(t, h.sink.drain())
	require.Equal(t, "hitl-policy-denyws.json", d.HITLPolicyName)
	require.Equalf(t, string(hitlservice.ActionDeny), d.HITLAction,
		"an explicit deny rule on the workspace toolset was not honoured: %q", d.HITLAction)

	require.Empty(t, h.asker.seen(), "a denied call must not consult the approver: there is nothing to approve")

	msg, ok := out.(string)
	require.Truef(t, ok, "a denied call must return the soft denial as a value the model can read, got %T", out)
	require.NotEmpty(t, msg)
}

// TestSystem_WorkspaceSearch_ShippedDenyFloorEnvelopesWithdrawTheToolset asserts the acpx/strict deny-floor presets withdraw workspace_search as a clean deny, not a crash or hang.
func TestSystem_WorkspaceSearch_ShippedDenyFloorEnvelopesWithdrawTheToolset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)

	for _, policy := range []string{"hitl-policy-acpx.json", "hitl-policy-strict.json"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()
			h.asker.reset()

			out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
			require.NoError(t, err)

			d := h.decisionFor(t, h.sink.drain())
			require.Equal(t, policy, d.HITLPolicyName)
			require.Equalf(t, string(hitlservice.ActionDeny), d.HITLAction,
				"%s has a deny floor and no workspace rule, so the call must be denied — if it is allowed, the pinned FILE is not what decided", policy)
			require.IsType(t, "", out, "%s: a denied call returns the deny message", policy)
			require.Empty(t, h.asker.seen(), "%s: a deny floor has no operator to prompt", policy)
		})
	}
}

// TestSystem_WorkspaceSearch_PresetUpgradeDeliversTheWorkspaceRule asserts the upgrade path adds the workspace rule to an existing install's untouched preset.
func TestSystem_WorkspaceSearch_PresetUpgradeDeliversTheWorkspaceRule(t *testing.T) {
	contenoxDir := filepath.Join(t.TempDir(), ".contenox")

	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))
	state := map[string]string{}
	for _, p := range HITLPolicyPresets {
		require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, p.Name), []byte(previous), 0o644))
		state[p.Name] = presetSHA(previous)
	}
	writePresetState(contenoxDir, state)

	stale, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	require.Emptyf(t, stale, "untouched presets were treated as hand-edited and held back: %v", stale)

	for _, name := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		raw, err := os.ReadFile(filepath.Join(contenoxDir, name))
		require.NoError(t, err)
		require.Containsf(t, string(raw), `"workspace"`,
			"%s was not upgraded, so every existing install gets an approval prompt per workspace_search", name)

		var doc struct {
			Rules []struct {
				Tools  string `json:"tools"`
				Action string `json:"action"`
			} `json:"rules"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		var found bool
		for _, r := range doc.Rules {
			if r.Tools == searchtool.ToolsProviderName {
				found = true
				require.Equalf(t, "allow", r.Action, "%s: the workspace rule is %q, not allow", name, r.Action)
			}
		}
		require.Truef(t, found, "%s: no rule for the %q toolset", name, searchtool.ToolsProviderName)
	}
}

// TestSystem_WorkspaceSearch_PreStateFileInstallIsNamedNotSilentlyRewritten asserts a pre-state-file install with a stale, unprovable preset is left untouched but named (via doctor/beam's startup line), rather than silently overwritten or silently nagging forever.
func TestSystem_WorkspaceSearch_PreStateFileInstallIsNamedNotSilentlyRewritten(t *testing.T) {
	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))

	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "hitl-policy-default.json"), []byte(previous), 0o644))
	require.NoFileExists(t, filepath.Join(contenoxDir, presetStateFile))

	stale, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	require.Containsf(t, stale, "hitl-policy-default.json",
		"the upgrade did not even NAME the preset it declined to refresh")

	raw, err := os.ReadFile(filepath.Join(contenoxDir, "hitl-policy-default.json"))
	require.NoError(t, err)
	require.Equal(t, previous, string(raw),
		"an unprovable envelope must survive the upgrade byte for byte")

	detected := stalePolicyPresets([]string{contenoxDir}, nil)
	require.Len(t, detected, 1)
	require.Equal(t, "hitl-policy-default.json", detected[0].Name)
	require.Containsf(t, detected[0].Toolsets, searchtool.ToolsProviderName,
		"the workspace toolset is exactly what this envelope will nag about")

	notice := stalePolicyNotice("hitl-policy-default.json", []string{contenoxDir}, nil)
	require.Contains(t, notice, searchtool.ToolsProviderName)
	require.Contains(t, notice, "stops for approval")
	require.Contains(t, notice, RefreshPoliciesCommand)

	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(contenoxDir), testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err := svc.Evaluate(context.Background(), searchtool.ToolsProviderName, searchtool.ToolSearch,
		map[string]any{"question": "where is retry backoff configured"})
	require.NoError(t, err)
	require.Equal(t, hitlservice.ActionApprove, r.Action)

	require.NoError(t, writeEmbeddedHITLPolicies(contenoxDir, true))
	require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{contenoxDir}, nil))
	require.Empty(t, stalePolicyPresets([]string{contenoxDir}, nil))

	svc = hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(contenoxDir), testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err = svc.Evaluate(context.Background(), searchtool.ToolsProviderName, searchtool.ToolSearch,
		map[string]any{"question": "where is retry backoff configured"})
	require.NoError(t, err)
	require.Equalf(t, hitlservice.ActionAllow, r.Action,
		"after the refresh a read toolset must stop asking")
}

// TestSystem_WorkspaceSearch_HandEditedPresetIsNeverOverwritten asserts a hand-edited preset is reported stale, never silently replaced by the upgrade.
func TestSystem_WorkspaceSearch_HandEditedPresetIsNeverOverwritten(t *testing.T) {
	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))

	_, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	edited := `{"default_action":"deny","rules":[]}`
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "hitl-policy-default.json"), []byte(edited), 0o644))

	stale, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	require.Contains(t, stale, "hitl-policy-default.json", "a hand-edited preset must be REPORTED as stale, not silently kept")

	raw, err := os.ReadFile(filepath.Join(contenoxDir, "hitl-policy-default.json"))
	require.NoError(t, err)
	require.Equal(t, edited, string(raw), "the operator's envelope was overwritten by the upgrade")
}

func ollamaReachable() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:11434", 750*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestSystem_WorkspaceSearch_LiveIndexAnswersWithCitationsUnderTheShippedPolicy asserts a real index queried through the real engine and shipped policy returns citations matching the files it indexed, using the same model route the CLI uses rather than a fake.
func TestSystem_WorkspaceSearch_LiveIndexAnswersWithCitationsUnderTheShippedPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live workspace_search e2e: needs a real embedding model")
	}
	if !ollamaReachable() {
		t.Skip("skipping live workspace_search e2e: no ollama daemon on 127.0.0.1:11434")
	}

	root := t.TempDir()
	files := map[string]string{
		"docs/retry.md": "# Retry policy\n\nWhen a request fails we wait before trying again.\n" +
			"The delay doubles after each attempt, which is called exponential backoff.\n" +
			"The ceiling is thirty seconds so a stuck caller never waits forever.\n",
		"docs/colors.md": "# Palette\n\nThe brand colour is a deep blue used for headings and links.\n" +
			"Secondary colours are grey for borders and amber for warnings.\n",
		"cook/soup.md": "# Soup\n\nChop the onions, sweat them in butter, add stock and simmer.\n" +
			"Season at the end, never at the start, or the salt concentrates.\n",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}

	h := newWSearchHarness(t, func(t *testing.T, db libdb.DBManager) {
		ctx := context.Background()
		store := runtimetypes.New(db.WithoutTransaction())
		require.NoError(t, clikv.SetString(ctx, store, "default-embed-model", "nomic-embed-text"))
		require.NoError(t, clikv.SetString(ctx, store, "default-embed-provider", "ollama"))
		// Must exist before the backend cycle runs, or no model resolves.
		require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
			ID:      "wsearch-e2e-ollama",
			Name:    "ollama",
			BaseURL: "http://localhost:11434",
			Type:    "ollama",
		}))
	}, func(o *chatOpts) {
		// Must run: it populates runtime state, without which embed calls fail.
		o.EffectiveSkipBackendCycle = false
	})

	store := runtimetypes.New(h.db.WithoutTransaction())
	require.Equal(t, "nomic-embed-text", h.engine.EmbeddingModel.Name,
		"the engine did not resolve the configured embedding model")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	svc := workspaceindex.New(
		store,
		workspaceindex.NewLLMRepoEmbedder(h.engine.Models, h.engine.EmbeddingModel.Name, h.engine.EmbeddingModel.Provider),
		ollamatokenizer.NewEstimateTokenizer(),
		workspaceindex.Config{EmbedModel: h.engine.EmbeddingModel.Name, EmbedProvider: h.engine.EmbeddingModel.Provider},
	)
	report, err := svc.Build(ctx, root, workspaceindex.BuildOptions{WorkspaceID: h.workspaceID})
	require.NoError(t, err, "the live index build failed — is nomic-embed-text pulled?")
	require.Greater(t, report.ChunksWritten, 0)
	t.Logf("live index: %d chunk(s), %d embed call(s), %v", report.ChunksWritten, report.EmbedCalls, report.Duration)

	policy := "hitl-policy-default.json"
	qctx := hitlservice.WithPolicyName(ctx, policy)
	h.sink.drain()
	h.asker.reset()

	started := time.Now()
	out, err := h.call(qctx, map[string]string{"question": "how long does it wait between retries"})
	require.NoError(t, err)
	t.Logf("live workspace_search round trip: %v", time.Since(started))

	d := h.decisionFor(t, h.sink.drain())
	require.Equal(t, string(hitlservice.ActionAllow), d.HITLAction)
	require.Equal(t, policy, d.HITLPolicyName)
	require.Empty(t, h.asker.seen())

	res, ok := out.(*searchtool.Result)
	require.Truef(t, ok, "result is %T, not the tool's payload", out)
	require.NotEmpty(t, res.Hits, "a live index returned no hits for a question its corpus answers")

	top := res.Hits[0]
	require.Equalf(t, "docs/retry.md", top.Path,
		"the top hit is %s, not the document that answers the question (scores: %s)", top.Path, wsearchScores(res))
	require.Greater(t, top.EndLine, 0)
	require.Equal(t, fmt.Sprintf("%s:%d-%d", top.Path, top.StartLine, top.EndLine), top.Citation)
	require.Contains(t, strings.ToLower(top.Text), "backoff")
	require.False(t, top.Stale, "nothing was edited, so no hit may be stale")

	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "retry.md"),
		[]byte("# Retry policy\n\nRewritten after indexing: the backoff ceiling is now sixty seconds.\n"), 0o644))

	out, err = h.call(qctx, map[string]string{"question": "how long does it wait between retries"})
	require.NoError(t, err)
	res, ok = out.(*searchtool.Result)
	require.True(t, ok)
	require.NotEmpty(t, res.Hits)
	require.Greaterf(t, res.Stale, 0, "the file was rewritten after indexing and no hit was marked stale — a hit whose file changed underneath is a lie")
	require.Containsf(t, res.Note, "stale", "the payload does not tell the model its citations moved: %q", res.Note)
}

func wsearchScores(res *searchtool.Result) string {
	var b strings.Builder
	for i, h := range res.Hits {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%.4f", h.Citation, h.Score)
	}
	return b.String()
}
