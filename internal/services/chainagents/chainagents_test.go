package chainagents_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// recordingTracker records the (operation, subject, error) of every report.
type recordingTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	op, subject string
	err         error
	kv          []any
}

func (r *recordingTracker) Start(_ context.Context, op, subject string, kv ...any) (func(error), func(string, any), func()) {
	kvCopy := append([]any(nil), kv...)
	return func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, trackedEvent{op: op, subject: subject, err: err, kv: kvCopy})
		},
		func(string, any) {},
		func() {}
}

// errorsFor returns the reported errors for one operation/subject pair.
func (r *recordingTracker) errorsFor(op, subject string) []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []error
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && ev.err != nil {
			out = append(out, ev.err)
		}
	}
	return out
}

var _ libtracker.ActivityTracker = (*recordingTracker)(nil)

func setupRegistry(t *testing.T) (context.Context, agentregistryservice.Service) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "chainagents.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, agentregistryservice.New(db)
}

// writeChain writes a minimal valid chain (id plus one task) into dir under name.
func writeChain(t *testing.T, dir, name, chainID string) string {
	t.Helper()
	chain := taskengine.TaskChainDefinition{
		ID:    chainID,
		Tasks: []taskengine.TaskDefinition{{ID: "reply", Handler: taskengine.HandleNoop, Print: "ok"}},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func mustGet(t *testing.T, ctx context.Context, agents agentregistryservice.Service, name string) *runtimetypes.Agent {
	t.Helper()
	agent, err := agents.GetByName(ctx, name)
	require.NoError(t, err, "agent %q should have been discovered", name)
	return agent
}

func TestUnit_Discover_FilenameConventionDeclaresAnAgent(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	declared := writeChain(t, dir, "chain-agent-reviewer.json", "reviewer")
	writeChain(t, dir, "helper.json", "helper")
	writeChain(t, dir, "agent-oldstyle.json", "oldstyle")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Created)

	_, err = agents.GetByName(ctx, "oldstyle")
	require.ErrorIs(t, err, libdb.ErrNotFound,
		"the pre-convention agent-* filename no longer declares an agent; rename the file to chain-agent-*")

	agent := mustGet(t, ctx, agents, "reviewer")
	require.Equal(t, runtimetypes.AgentKindChain, agent.Kind)
	require.True(t, agent.Enabled, "a freshly discovered chain agent is dispatchable immediately")
	require.NotNil(t, agent.Source)
	require.Equal(t, runtimetypes.AgentSourceDiscovered, *agent.Source)

	cfg, err := agent.ChainConfig()
	require.NoError(t, err)
	require.Equal(t, declared, cfg.Path, "the row names the absolute chain file the unit will run")
	require.Equal(t, "reviewer", cfg.ChainID)

	_, err = agents.GetByName(ctx, "helper")
	require.ErrorIs(t, err, libdb.ErrNotFound, "an undeclared chain is not an agent")
}

// Shipped agent-shaped chains are eligible by id even under a legacy disk
// name (an install that has not run `init --update` yet); shipped utility
// chains are not agents whatever their name.
func TestUnit_Discover_ShippedAgenticChainsAreEligibleByID(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "default-acp-chain.json", "chain-acp")
	writeChain(t, dir, "headless-acp-chain.json", "chain-acpx")
	writeChain(t, dir, "chain-compact-default.json", "chain-compact")
	writeChain(t, dir, "chain-fim-default.json", "chain-fim")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"chain-acp", "chain-acpx"}, res.Created)

	for _, utility := range []string{"chain-compact", "chain-fim"} {
		_, err := agents.GetByName(ctx, utility)
		require.ErrorIsf(t, err, libdb.ErrNotFound, "%s is a utility chain, not an agent template", utility)
	}
}

// The planner is eligible by its shipped chain id, and the agent NAME is the
// chain id verbatim — the chain-planner-default.json rename must not move
// `mission fire agent-planner` or an existing default-mission-agent config.
func TestUnit_Discover_PlannerNameIsStableAcrossTheRename(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	seeded := writeChain(t, dir, "chain-planner-default.json", "agent-planner")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-planner"}, res.Created,
		"the planner registers under its chain id, not its filename")

	cfg, err := mustGet(t, ctx, agents, "agent-planner").ChainConfig()
	require.NoError(t, err)
	require.Equal(t, seeded, cfg.Path)

	// The legacy seeded filename (pre `init --update`) resolves to the same
	// agent name: the row is rewritten to the new path, never renamed.
	legacyDir := t.TempDir()
	legacy := writeChain(t, legacyDir, "agent-planner.json", "agent-planner")
	res, err = chainagents.Discover(ctx, agents, legacyDir)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-planner"}, res.Updated)
	cfg, err = mustGet(t, ctx, agents, "agent-planner").ChainConfig()
	require.NoError(t, err)
	require.Equal(t, legacy, cfg.Path)
}

// DiscoverKept restricted to the stable roster: shipped chains and the
// planner register; a user-authored chain-agent-* chain is neither registered
// nor — once registered by an unrestricted pass — reconciled or disabled.
func TestUnit_DiscoverKept_StableRosterCut(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-planner-default.json", "agent-planner")
	writeChain(t, dir, "chain-agent-acp.json", "chain-acp")
	custom := writeChain(t, dir, "chain-agent-custom.json", "agent-custom")

	res, err := chainagents.DiscoverKept(ctx, agents, nil, chainagents.StableAgentName, dir)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"agent-planner", "chain-acp"}, res.Created,
		"the shipped planner and surface chains stay dispatchable without the beta roster")
	_, err = agents.GetByName(ctx, "agent-custom")
	require.ErrorIs(t, err, libdb.ErrNotFound, "a user-authored chain-agent-* chain is invisible without opt-in")

	// Registered while the roster was unrestricted...
	_, err = chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.True(t, mustGet(t, ctx, agents, "agent-custom").Enabled)

	// ...then the file vanishes and a restricted pass runs: the row is outside
	// the pass entirely — untouched, not disabled.
	require.NoError(t, os.Remove(custom))
	res, err = chainagents.DiscoverKept(ctx, agents, nil, chainagents.StableAgentName, dir)
	require.NoError(t, err)
	require.Empty(t, res.Disabled)
	require.True(t, mustGet(t, ctx, agents, "agent-custom").Enabled,
		"a restricted pass never scanned for the row, so it must not judge it vanished")
}

// A repeat pass must not even touch updated_at.
func TestUnit_Discover_IsIdempotent(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-reviewer.json", "reviewer")
	writeChain(t, dir, "chain-agent-acp.json", "chain-acp")

	first, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"reviewer", "chain-acp"}, first.Created)

	before := []*runtimetypes.Agent{
		mustGet(t, ctx, agents, "reviewer"),
		mustGet(t, ctx, agents, "chain-acp"),
	}

	second, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, second.Created)
	require.Empty(t, second.Updated)
	require.Empty(t, second.Disabled)
	require.ElementsMatch(t, []string{"reviewer", "chain-acp"}, second.Unchanged)

	for _, was := range before {
		now := mustGet(t, ctx, agents, was.Name)
		require.Equal(t, was.ID, now.ID)
		require.Equal(t, was.UpdatedAt, now.UpdatedAt, "a repeat pass must not even touch updated_at")
		require.JSONEq(t, string(was.ConfigJSON), string(now.ConfigJSON))
	}

	third, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, third.Created)
	require.Empty(t, third.Updated)
}

func TestUnit_Discover_MovedChainIsRewritten(t *testing.T) {
	ctx, agents := setupRegistry(t)
	first := t.TempDir()
	writeChain(t, first, "chain-agent-reviewer.json", "reviewer")
	_, err := chainagents.Discover(ctx, agents, first)
	require.NoError(t, err)

	second := t.TempDir()
	moved := writeChain(t, second, "chain-agent-reviewer.json", "reviewer")
	res, err := chainagents.Discover(ctx, agents, second)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Updated)

	cfg, err := mustGet(t, ctx, agents, "reviewer").ChainConfig()
	require.NoError(t, err)
	require.Equal(t, moved, cfg.Path)
}

func TestUnit_Discover_VanishedChainIsDisabledNotDeleted(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	path := writeChain(t, dir, "chain-agent-reviewer.json", "reviewer")

	_, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.True(t, mustGet(t, ctx, agents, "reviewer").Enabled)

	require.NoError(t, os.Remove(path))
	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Disabled)

	agent := mustGet(t, ctx, agents, "reviewer")
	require.False(t, agent.Enabled)

	_, err = agentregistryservice.ResolveForSpawn(ctx, agents, "reviewer")
	require.ErrorIs(t, err, agentregistryservice.ErrAgentDisabled,
		"a vanished chain must be refused by the same judgement every other disabled agent is")

	again, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, again.Disabled)
}

func TestUnit_Discover_NeverReEnablesAnExistingRow(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-reviewer.json", "reviewer")

	_, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)

	agent := mustGet(t, ctx, agents, "reviewer")
	agent.Enabled = false
	require.NoError(t, agents.Update(ctx, agent))

	_, err = chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.False(t, mustGet(t, ctx, agents, "reviewer").Enabled,
		"discovery must not undo an operator's decision on the next startup")
}

func TestUnit_Discover_LeavesForeignRowsAlone(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-reviewer.json", "reviewer")

	manual := &runtimetypes.Agent{Name: "reviewer", Enabled: true}
	source := runtimetypes.AgentSourceManual
	manual.Source = &source
	require.NoError(t, manual.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "some-other-agent",
	}))
	require.NoError(t, agents.Create(ctx, manual))

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Skipped)

	got := mustGet(t, ctx, agents, "reviewer")
	require.Equal(t, runtimetypes.AgentKindExternalACP, got.Kind, "a hand-registered agent outranks a filename convention")
	require.Equal(t, manual.UpdatedAt, got.UpdatedAt)
}

// The same chain id present in two roots resolves to the first one listed.
func TestUnit_Discover_WorkspaceShadowsHome(t *testing.T) {
	ctx, agents := setupRegistry(t)
	workspace, home := t.TempDir(), t.TempDir()
	winner := writeChain(t, workspace, "chain-agent-reviewer.json", "reviewer")
	writeChain(t, home, "chain-agent-reviewer.json", "reviewer")

	res, err := chainagents.Discover(ctx, agents, workspace, home)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Created)

	cfg, err := mustGet(t, ctx, agents, "reviewer").ChainConfig()
	require.NoError(t, err)
	require.Equal(t, winner, cfg.Path)
}

// A missing root is skipped, not created; an unparseable file skips the pass, not fails it.
func TestUnit_Discover_ToleratesMissingAndBrokenInput(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-good.json", "good")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-agent-broken.json"), []byte("{not json"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-agent-empty.json"), []byte(`{"id":"empty"}`), 0o600))

	absent := filepath.Join(t.TempDir(), "does-not-exist")
	res, err := chainagents.Discover(ctx, agents, absent, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"good"}, res.Created)
	require.NoDirExists(t, absent, "discovery reads the operator's directories, it does not create them")
}

func TestUnit_Discover_RequiresARegistry(t *testing.T) {
	_, err := chainagents.Discover(context.Background(), nil, t.TempDir())
	require.Error(t, err)
}

func TestUnit_Discover_LintFailingChainIsSkippedAndDisabled(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-worker.json", "worker")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"worker"}, res.Created)
	require.True(t, mustGet(t, ctx, agents, "worker").Enabled)

	broken := taskengine.TaskChainDefinition{
		ID:    "worker",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	}
	data, err := json.Marshal(broken)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-agent-worker.json"), data, 0o600))

	res, err = chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, res.Created)
	require.Empty(t, res.Updated)
	require.Equal(t, []string{"worker"}, res.Disabled,
		"a chain the linter refuses must be disabled like a vanished file")
	require.False(t, mustGet(t, ctx, agents, "worker").Enabled)

	writeChain(t, dir, "chain-agent-worker.json", "worker")
	_, err = chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.False(t, mustGet(t, ctx, agents, "worker").Enabled)
}

// The same directory listed twice as roots must not double the diagnostics.
func TestUnit_Discover_RepeatedRootIsWalkedOnce(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-good.json", "good")
	broken, err := json.Marshal(taskengine.TaskChainDefinition{
		ID:    "brokenagent",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-agent-broken.json"), broken, 0o600))

	tracker := &recordingTracker{}
	res, err := chainagents.DiscoverWithTracker(ctx, agents, tracker, dir, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"good"}, res.Created, "a repeated root must not change what is discovered")
	reported := tracker.errorsFor("discover", "chain_agent")
	require.Len(t, reported, 1,
		"one broken chain file must produce exactly one report, however many times its directory was listed")
	require.ErrorIs(t, reported[0], taskengine.ErrChainLint,
		"the report must carry WHY the file was refused, not just that it was")
	require.Contains(t, reported[0].Error(), "chain file fails validation")
}

func TestUnit_Discover_ReportsRefusedChainAndDisabledAgent(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "chain-agent-worker.json", "worker")

	tracker := &recordingTracker{}
	_, err := chainagents.DiscoverWithTracker(ctx, agents, tracker, dir)
	require.NoError(t, err)
	require.Empty(t, tracker.errorsFor("discover", "chain_agent"), "a clean pass reports nothing")
	require.Empty(t, tracker.errorsFor("disable", "chain_agent"))

	broken, err := json.Marshal(taskengine.TaskChainDefinition{
		ID:    "worker",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-agent-worker.json"), broken, 0o600))

	tracker = &recordingTracker{}
	res, err := chainagents.DiscoverWithTracker(ctx, agents, tracker, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"worker"}, res.Disabled)

	refused := tracker.errorsFor("discover", "chain_agent")
	require.Len(t, refused, 1, "the refused chain file is reported as it is skipped")
	require.ErrorIs(t, refused[0], taskengine.ErrChainLint)

	disabled := tracker.errorsFor("disable", "chain_agent")
	require.Len(t, disabled, 1, "disabling an agent whose file is gone or broken is reported")
	require.Contains(t, disabled[0].Error(), "chain agent disabled")
}

func TestUnit_Discover_WithoutTrackerStillRuns(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	broken, err := json.Marshal(taskengine.TaskChainDefinition{
		ID:    "brokenagent",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chain-agent-broken.json"), broken, 0o600))
	writeChain(t, dir, "chain-agent-good.json", "good")

	res, err := chainagents.DiscoverWithTracker(ctx, agents, nil, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"good"}, res.Created)
}
