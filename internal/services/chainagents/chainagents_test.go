package chainagents_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chainagents"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// recordingTracker records the (operation, subject, error) of every report so a
// test can assert WHAT discovery said, not merely that it logged something.
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

// writeChain writes a minimal VALID chain (an id plus one task — what the shared
// chain walker requires before it will list a file at all) into dir under name.
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

// TestUnit_Discover_FilenameConventionDeclaresAnAgent is the core of "naming the
// file IS the declaration": a chain called agent-*.json becomes a dispatchable
// agent with no registration step, and a chain that is not named that way — even
// a perfectly good one sitting beside it — does not.
func TestUnit_Discover_FilenameConventionDeclaresAnAgent(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	declared := writeChain(t, dir, "agent-reviewer.json", "reviewer")
	writeChain(t, dir, "helper.json", "helper")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Created)

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

// TestUnit_Discover_ShippedAgenticChainsAreEligibleByID pins the second
// convention: the agent-shaped chains the runtime ships are eligible under
// whatever filename they were installed as, and the utility chains shipped
// alongside them are not — a chain the runtime calls on data the runtime built
// is a subroutine, not a unit.
func TestUnit_Discover_ShippedAgenticChainsAreEligibleByID(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	// Shipped under their real installed filenames, which differ from their ids.
	writeChain(t, dir, "default-chain.json", "chain-contenox")
	writeChain(t, dir, "default-acp-chain.json", "chain-acp")
	writeChain(t, dir, "headless-acp-chain.json", "chain-acpx")
	// Utility chains shipped in the same directory.
	writeChain(t, dir, "chain-compact.json", "chain-compact")
	writeChain(t, dir, "default-fim-chain.json", "chain-fim")
	writeChain(t, dir, "default-run-chain.json", "chain-run")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"chain-contenox", "chain-acp", "chain-acpx"}, res.Created)

	for _, utility := range []string{"chain-compact", "chain-fim", "chain-run"} {
		_, err := agents.GetByName(ctx, utility)
		require.ErrorIsf(t, err, libdb.ErrNotFound, "%s is a utility chain, not an agent template", utility)
	}
}

// TestUnit_Discover_IsIdempotent is the acceptance for "running twice changes
// nothing". It is asserted on the stored row rather than on the return value,
// because the failure mode worth catching is a blind Update that rewrites
// updated_at on every startup — which would look like a no-op from the outside
// while churning the table forever.
func TestUnit_Discover_IsIdempotent(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-reviewer.json", "reviewer")
	writeChain(t, dir, "default-acp-chain.json", "chain-acp")

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

	// And a third pass over the same tree is still a no-op.
	third, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, third.Created)
	require.Empty(t, third.Updated)
}

// TestUnit_Discover_MovedChainIsRewritten covers the other half of idempotency:
// when the fact on disk really did change, the row follows it.
func TestUnit_Discover_MovedChainIsRewritten(t *testing.T) {
	ctx, agents := setupRegistry(t)
	first := t.TempDir()
	writeChain(t, first, "agent-reviewer.json", "reviewer")
	_, err := chainagents.Discover(ctx, agents, first)
	require.NoError(t, err)

	second := t.TempDir()
	moved := writeChain(t, second, "agent-reviewer.json", "reviewer")
	res, err := chainagents.Discover(ctx, agents, second)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Updated)

	cfg, err := mustGet(t, ctx, agents, "reviewer").ChainConfig()
	require.NoError(t, err)
	require.Equal(t, moved, cfg.Path)
}

// TestUnit_Discover_VanishedChainIsDisabledNotDeleted documents what happens to
// an agent whose chain file went away: the row survives (mission records and
// telemetry still point at it) but is disabled, so the ONE shared spawn-path
// judgement refuses it with a message that names the remedy. It is never left as
// a phantom that would spawn and then fail.
func TestUnit_Discover_VanishedChainIsDisabledNotDeleted(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	path := writeChain(t, dir, "agent-reviewer.json", "reviewer")

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

	// Still idempotent afterwards: a second pass reports nothing new to disable.
	again, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, again.Disabled)
}

// TestUnit_Discover_NeverReEnablesAnExistingRow pins the deliberate asymmetry:
// discovery brings a row into existence enabled, but never moves Enabled upward
// afterwards, so an operator's `contenox agent disable` survives every restart.
func TestUnit_Discover_NeverReEnablesAnExistingRow(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-reviewer.json", "reviewer")

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

// TestUnit_Discover_LeavesForeignRowsAlone: a name already held by an agent this
// package does not own is not clobbered, whatever is on disk.
func TestUnit_Discover_LeavesForeignRowsAlone(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-reviewer.json", "reviewer")

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

// TestUnit_Discover_WorkspaceShadowsHome pins root precedence: the same chain id
// present in two roots resolves to the first one listed, the way every other
// contenox config file resolves workspace-before-home.
func TestUnit_Discover_WorkspaceShadowsHome(t *testing.T) {
	ctx, agents := setupRegistry(t)
	workspace, home := t.TempDir(), t.TempDir()
	winner := writeChain(t, workspace, "agent-reviewer.json", "reviewer")
	writeChain(t, home, "agent-reviewer.json", "reviewer")

	res, err := chainagents.Discover(ctx, agents, workspace, home)
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer"}, res.Created)

	cfg, err := mustGet(t, ctx, agents, "reviewer").ChainConfig()
	require.NoError(t, err)
	require.Equal(t, winner, cfg.Path)
}

// TestUnit_Discover_ToleratesMissingAndBrokenInput: a root that does not exist is
// skipped without being created, and an unparseable file in a real root is
// skipped without failing the pass — the same fail-soft the shared chain walker
// already applies everywhere else.
func TestUnit_Discover_ToleratesMissingAndBrokenInput(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-good.json", "good")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-broken.json"), []byte("{not json"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-empty.json"), []byte(`{"id":"empty"}`), 0o600))

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

// TestUnit_Discover_LintFailingChainIsSkippedAndDisabled: discovery vets every
// chain before seeding (via taskchainservice.Get's linter), so a chain the
// linter refuses (1) never seeds the registry and (2) gets its existing
// discovered row DISABLED through the same path a vanished file takes — sticky:
// even after the file is fixed, re-enabling is the operator's explicit verb.
func TestUnit_Discover_LintFailingChainIsSkippedAndDisabled(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-worker.json", "worker")

	res, err := chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"worker"}, res.Created)
	require.True(t, mustGet(t, ctx, agents, "worker").Enabled)

	// Break the chain in place: still parseable, still has an id and tasks
	// (so the walker lists it), but the linter refuses it (unknown handler).
	broken := taskengine.TaskChainDefinition{
		ID:    "worker",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	}
	data, err := json.Marshal(broken)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-worker.json"), data, 0o600))

	res, err = chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.Empty(t, res.Created)
	require.Empty(t, res.Updated)
	require.Equal(t, []string{"worker"}, res.Disabled,
		"a chain the linter refuses must be disabled like a vanished file")
	require.False(t, mustGet(t, ctx, agents, "worker").Enabled)

	// Fixing the file does NOT auto re-enable: Discover never moves Enabled
	// upward, so the operator's `contenox agent enable` stays the one verb.
	writeChain(t, dir, "agent-worker.json", "worker")
	_, err = chainagents.Discover(ctx, agents, dir)
	require.NoError(t, err)
	require.False(t, mustGet(t, ctx, agents, "worker").Enabled)
}

// TestUnit_Discover_RepeatedRootIsWalkedOnce pins the dedupe. roots is a
// PRECEDENCE list, so the same directory listed twice can add nothing — the
// first pass claims every agent it provides and the second would lose all of
// them to itself. What the duplication DID produce was duplicated diagnostics:
// every surface whose workspace directory is also ~/.contenox (beam, and
// `contenox acp` outside a project) passed the same path as both roots, and an
// operator with one lint-failing chain saw its warning twice on every single
// startup — which reads as two broken files, not one.
func TestUnit_Discover_RepeatedRootIsWalkedOnce(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-good.json", "good")
	// Parseable and listable, but the linter refuses it — the shape that warns.
	broken, err := json.Marshal(taskengine.TaskChainDefinition{
		ID:    "brokenagent",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-broken.json"), broken, 0o600))

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

// TestUnit_Discover_ReportsRefusedChainAndDisabledAgent proves both diagnostics
// this pass produces reach the tracker — the ONLY instrumentation seam here, so
// an operator's view of "why is my agent missing" lives or dies by these two
// reports. A chain the linter refuses is reported as it is skipped, and the
// agent it had already seeded is reported again as it is disabled.
func TestUnit_Discover_ReportsRefusedChainAndDisabledAgent(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	writeChain(t, dir, "agent-worker.json", "worker")

	tracker := &recordingTracker{}
	_, err := chainagents.DiscoverWithTracker(ctx, agents, tracker, dir)
	require.NoError(t, err)
	require.Empty(t, tracker.errorsFor("discover", "chain_agent"), "a clean pass reports nothing")
	require.Empty(t, tracker.errorsFor("disable", "chain_agent"))

	// Break the file the agent points at: parseable and listable, refused by the
	// linter — the shape that reports twice, once per diagnostic.
	broken, err := json.Marshal(taskengine.TaskChainDefinition{
		ID:    "worker",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-worker.json"), broken, 0o600))

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

// TestUnit_Discover_WithoutTrackerStillRuns pins the nil-degrades-to-noop
// contract: instrumentation is an observer here, never a dependency the pass
// needs to reconcile the registry.
func TestUnit_Discover_WithoutTrackerStillRuns(t *testing.T) {
	ctx, agents := setupRegistry(t)
	dir := t.TempDir()
	broken, err := json.Marshal(taskengine.TaskChainDefinition{
		ID:    "brokenagent",
		Tasks: []taskengine.TaskDefinition{{ID: "one", Handler: "prompt"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-broken.json"), broken, 0o600))
	writeChain(t, dir, "agent-good.json", "good")

	res, err := chainagents.DiscoverWithTracker(ctx, agents, nil, dir)
	require.NoError(t, err)
	require.Equal(t, []string{"good"}, res.Created)
}
