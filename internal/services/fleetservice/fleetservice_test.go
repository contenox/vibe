package fleetservice

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/errdefs"
	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// ─── fakeManager: a hand-rolled agentinstance.Manager double ───────────────
//
// It records every call fleetservice.Dispatch/Stop/Cancel makes so a test can
// assert the orchestration (teardown-on-failure, cancel fan-out) without
// spawning a real subprocess — the agentinstance package's own tests already
// cover the kernel's real behavior; this package tests the POLICY wrapped
// around it.

type cancelCall struct {
	instanceID string
	sessionID  libacp.SessionID
}

type fakeManager struct {
	mu sync.Mutex

	startID       string
	startErr      error
	startCalls    []string
	startedAgents []*runtimetypes.Agent

	openID    libacp.SessionID
	openErr   error
	openSpecs []agentinstance.SessionSpec

	promptErr          error
	promptCalls        int
	promptBlocks       []libacp.ContentBlock   // blocks of the MOST RECENT prompt
	promptBlocksByCall [][]libacp.ContentBlock // blocks of every prompt, in order
	// onPrompt, when set, runs on each Prompt call (call is 1-based) BEFORE it
	// returns — the hook a test uses to make the "unit" file a mission fact so no
	// nudge follows. Runs under no lock.
	onPrompt func(call int)
	// agentText is what SessionAgentText returns for any (instance, session) — the
	// unit's "last words" fleetservice quotes into a runtime-filed blocker.
	agentText string

	stopCalls []string

	cancelErr   error
	cancelCalls []cancelCall

	statuses map[string]agentinstance.InstanceStatus
}

func (m *fakeManager) Start(_ context.Context, agentName, _ string) (string, error) {
	m.mu.Lock()
	m.startCalls = append(m.startCalls, agentName)
	m.mu.Unlock()
	if m.startErr != nil {
		return "", m.startErr
	}
	return m.startID, nil
}

// StartResolved records the spawn under the resolved record's NAME, so every
// existing starts() assertion keeps reading the same way. Dispatch calls this one
// — it already resolved the agent to make the Enabled decision, and re-resolving
// by name in the kernel would reopen the TOCTOU window that check exists to close.
func (m *fakeManager) StartResolved(_ context.Context, agent *runtimetypes.Agent, _ string) (string, error) {
	m.mu.Lock()
	name := ""
	if agent != nil {
		name = agent.Name
	}
	m.startCalls = append(m.startCalls, name)
	m.startedAgents = append(m.startedAgents, agent)
	m.mu.Unlock()
	if m.startErr != nil {
		return "", m.startErr
	}
	return m.startID, nil
}

func (m *fakeManager) OpenSession(_ context.Context, _ string, spec agentinstance.SessionSpec) (libacp.SessionID, error) {
	m.mu.Lock()
	m.openSpecs = append(m.openSpecs, spec)
	m.mu.Unlock()
	if m.openErr != nil {
		return "", m.openErr
	}
	return m.openID, nil
}

func (m *fakeManager) Prompt(_ context.Context, _ string, _ libacp.SessionID, blocks []libacp.ContentBlock) (libacp.StopReason, error) {
	m.mu.Lock()
	m.promptCalls++
	call := m.promptCalls
	m.promptBlocks = blocks
	m.promptBlocksByCall = append(m.promptBlocksByCall, blocks)
	hook := m.onPrompt
	m.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return libacp.StopReasonEndTurn, m.promptErr
}

// SessionAgentText satisfies fleetservice's optional sessionTextReader capability
// (reached by type assertion), returning the configured agentText for any
// (instance, session) so a test can assert the runtime-filed blocker quotes the
// unit's last words.
func (m *fakeManager) SessionAgentText(_ string, _ libacp.SessionID) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agentText, true
}

func (m *fakeManager) DeliverToSession(context.Context, libacp.SessionID, libacp.SessionNotification) error {
	return nil
}

func (m *fakeManager) Stop(instanceID string) error {
	m.mu.Lock()
	m.stopCalls = append(m.stopCalls, instanceID)
	m.mu.Unlock()
	return nil
}

func (m *fakeManager) Cancel(instanceID string, sessionID libacp.SessionID) error {
	m.mu.Lock()
	m.cancelCalls = append(m.cancelCalls, cancelCall{instanceID: instanceID, sessionID: sessionID})
	m.mu.Unlock()
	return m.cancelErr
}

func (m *fakeManager) Get(instanceID string) (agentinstance.InstanceStatus, error) {
	st, ok := m.statuses[instanceID]
	if !ok {
		return agentinstance.InstanceStatus{}, fmt404(instanceID)
	}
	return st, nil
}

func fmt404(instanceID string) error {
	return &notFoundErr{instanceID: instanceID}
}

type notFoundErr struct{ instanceID string }

func (e *notFoundErr) Error() string { return "agentinstance: " + e.instanceID + ": not found" }
func (e *notFoundErr) Unwrap() error { return agentinstance.ErrNotFound }

// The remaining Manager methods are unused by fleetservice; no-op them to
// satisfy the interface.
func (m *fakeManager) Attach(context.Context, string, libacp.SessionID, agentinstance.Viewer) (bool, error) {
	return false, nil
}
func (m *fakeManager) Detach(string, libacp.SessionID, string) error { return nil }
func (m *fakeManager) List(context.Context) ([]agentinstance.FleetEntry, error) {
	return nil, nil
}
func (m *fakeManager) CloseSession(string, libacp.SessionID) error { return nil }
func (m *fakeManager) SetConfigOption(context.Context, string, libacp.SessionID, string, libacp.SessionConfigOptionValue) error {
	return nil
}
func (m *fakeManager) SessionConfigOptions(string, libacp.SessionID) ([]libacp.SessionConfigOption, error) {
	return nil, nil
}
func (m *fakeManager) AvailableCommands(string, libacp.SessionID) ([]libacp.AvailableCommand, error) {
	return nil, nil
}
func (m *fakeManager) Close() error { return nil }

func (m *fakeManager) starts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.startCalls...)
}

func (m *fakeManager) stops() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.stopCalls...)
}

func (m *fakeManager) prompts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.promptCalls
}

func (m *fakeManager) promptCallBlocks() [][]libacp.ContentBlock {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]libacp.ContentBlock, len(m.promptBlocksByCall))
	copy(out, m.promptBlocksByCall)
	return out
}

func (m *fakeManager) cancels() []cancelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cancelCall(nil), m.cancelCalls...)
}

var _ agentinstance.Manager = (*fakeManager)(nil)

// ─── setup helpers ──────────────────────────────────────────────────────────

// setupRegistryDB gives a test a real sqlite-backed agentregistryservice /
// missionservice pair — both are validated-CRUD registries this package
// depends on for real, so exercising them for real is cheap (sqlite, no
// subprocess) and catches drift the store's own validate() would reject.
func setupRegistryDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleetservice.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// registerAgent declares an external_acp agent named name with the given
// enabled flag, via the real registry service.
func registerAgent(t *testing.T, ctx context.Context, agents agentregistryservice.Service, name string, enabled bool) {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: enabled}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "/bin/true",
	}))
	require.NoError(t, agents.Create(ctx, agent))
}

// waitMissionSettled blocks until the detached dispatch goroutine has run to
// completion for a BARE unit (one that files no mission fact): it nudges once and
// then files exactly one runtime blocker, which is the goroutine's LAST durable
// write, so its appearance means the goroutine is done — and t.Cleanup's db close
// cannot race it. Use it after a successful Dispatch whose fake unit reports
// nothing, so the test tears down against a quiescent goroutine.
func waitMissionSettled(t *testing.T, missions missionservice.Service, missionID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		reps, err := missions.ListReports(context.Background(), missionID, 5)
		return err == nil && len(reps) > 0
	}, 5*time.Second, 20*time.Millisecond, "dispatch goroutine never settled (no runtime blocker filed)")
}

// countingRegistry wraps a real agentregistryservice.Service and counts
// GetByName calls — the read every spawn path used to make TWICE (once for the
// Enabled decision, once inside the kernel's Start).
type countingRegistry struct {
	agentregistryservice.Service
	mu     sync.Mutex
	byName int
}

func (r *countingRegistry) GetByName(ctx context.Context, name string) (*runtimetypes.Agent, error) {
	r.mu.Lock()
	r.byName++
	r.mu.Unlock()
	return r.Service.GetByName(ctx, name)
}

func (r *countingRegistry) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byName
}

// ─── Dispatch: Enabled policy ───────────────────────────────────────────────

func TestFleetService_Dispatch_DisabledAgentRefused(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", false)

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, nil, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default"})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict, "a disabled agent is refused as a 4xx conflict")
	require.Contains(t, err.Error(), "disabled")
	require.Empty(t, man.starts(), "a refused dispatch must never bring an instance up")
}

func TestFleetService_Dispatch_UnknownAgentPropagatesNotFound(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	// No agent registered.

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, nil, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "ghost", Intent: "do the thing", HITLPolicyName: "default"})
	require.Error(t, err)
	require.ErrorIs(t, err, libdb.ErrNotFound)
	require.Empty(t, man.starts())
}

// ─── Dispatch: happy path ───────────────────────────────────────────────────

// TestFleetService_Dispatch_HappyPath drives the full flow: an instance comes
// up, a session opens, the mission is created carrying the request's envelope
// and bound to both fresh ids, and the intent runs as the unit's first turn.
// Every dispatch is a mission now, so there is no "with mission" qualifier
// left to distinguish this from any other successful dispatch.
func TestFleetService_Dispatch_HappyPath(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-7", openID: "sess-7"}
	// A unit that files a report on its first turn — the happy path, no nudge —
	// over the same store a real dispatched unit writes to.
	man.onPrompt = func(call int) {
		if call != 1 {
			return
		}
		if ms, _ := missions.List(context.Background(), nil, 10); len(ms) == 1 {
			_ = missions.AddReport(context.Background(), ms[0].ID, &missionservice.Report{
				Kind: missionservice.ReportKindProgress, Summary: "shipping",
			})
		}
	}

	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{})

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "ship the board",
		HITLPolicyName: "default",
	})
	require.NoError(t, err)
	require.Equal(t, "inst-7", result.InstanceID)
	require.Equal(t, "sess-7", result.SessionID)
	require.NotEmpty(t, result.MissionID)
	require.Equal(t, []string{"runner"}, man.starts())

	// cwd defaulted to the project root (no allowlist configured).
	require.Len(t, man.openSpecs, 1)
	require.Equal(t, "/project/root", man.openSpecs[0].Cwd)

	m, err := missions.Get(ctx, result.MissionID)
	require.NoError(t, err)
	require.Equal(t, "ship the board", m.Intent, "the intent is stored CLEAN — the preamble is wire-only")
	require.Equal(t, "runner", m.AgentName)
	require.Equal(t, "default", m.HITLPolicyName, "the mission carries the request's envelope")
	require.Equal(t, "sess-7", m.SessionID, "bound to the session Dispatch just opened")
	require.Equal(t, "inst-7", m.InstanceID, "bound to the instance Dispatch just started")

	// The reporting unit is not nudged: exactly one turn ever runs.
	require.Eventually(t, func() bool {
		reps, _ := missions.ListReports(ctx, result.MissionID, 5)
		return len(reps) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, 1, man.prompts(), "a unit that reported is not nudged")
	require.Empty(t, man.stops(), "a successful dispatch must never stop the instance it just brought up")

	// The intent IS the prompt, run CLEAN behind the wire-only preamble: turn one
	// is [preamble, intent], and the intent block flattens to exactly the request.
	blocks := man.promptCallBlocks()
	require.Len(t, blocks, 1)
	require.Len(t, blocks[0], 2, "the first turn is the preamble ahead of the intent")
	intentText, _ := libacp.FlattenContent(blocks[0][1:])
	require.Equal(t, "ship the board", intentText, "the intent runs as the unit's first turn")
}

// TestFleetService_Dispatch_ResolvesTheAgentExactlyOnce is the service half of
// closing the spawn-path TOCTOU. Dispatch resolves the declared agent in order to
// make the Enabled decision; it must then hand THAT record to the kernel rather
// than a name for the kernel to re-resolve. Two reads meant the check was made
// against the first and the spawn proceeded from the second, so an agent disabled
// in between still spawned — a hole in the exact check this path exists to enforce.
// (The kernel half — StartResolved performing no read of its own — is pinned in
// agentinstance.)
func TestFleetService_Dispatch_ResolvesTheAgentExactlyOnce(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	counting := &countingRegistry{Service: agents}
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-once", openID: "sess-once"}
	svc := New(man, counting, missions, nil, "/project/root", nil)

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default",
	})
	require.NoError(t, err)
	require.Equal(t, "inst-once", result.InstanceID)
	require.Equal(t, 1, counting.reads(), "one dispatch, one registry read")

	require.Len(t, man.startedAgents, 1)
	spawned := man.startedAgents[0]
	require.NotNil(t, spawned, "the kernel is handed the record, not a name to re-resolve")
	require.Equal(t, "runner", spawned.Name)
	require.True(t, spawned.Enabled, "the bytes that were judged are the bytes that are spawned")

	// The bare unit's shepherding (nudge + blocker) reads only the mission store,
	// never the registry — so one read stands even after the goroutine runs. Settle
	// it so its writes do not race t.Cleanup.
	waitMissionSettled(t, missions, result.MissionID)
	require.Equal(t, 1, counting.reads(), "shepherding a mute unit adds no registry read")
}

func TestFleetService_Dispatch_MissingAgentNameRejected(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)

	svc := New(&fakeManager{}, agents, nil, nil, "/project/root", nil)
	_, err := svc.Dispatch(ctx, DispatchRequest{})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrMissingParameter)
}

// TestFleetService_Dispatch_IntentRequiredRejected proves the intent — the
// content of the unit's first turn — cannot be empty even when every other
// field is valid.
func TestFleetService_Dispatch_IntentRequiredRejected(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-4", openID: "sess-4"}
	svc := New(man, agents, nil, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "runner", HITLPolicyName: "default"})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrMissingParameter)
	require.Empty(t, man.starts(), "rejected before any instance is brought up")
}

// TestFleetService_Dispatch_EnvelopeRequiredRejected proves the HITL policy
// name — the mission's envelope — cannot be empty: a mission with no bounds
// is exactly what mission mode must not permit.
func TestFleetService_Dispatch_EnvelopeRequiredRejected(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-4", openID: "sess-4"}
	svc := New(man, agents, nil, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "runner", Intent: "do the thing"})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrMissingParameter)
	require.Empty(t, man.starts(), "rejected before any instance is brought up")
}

// ─── Dispatch: teardown-on-failure ──────────────────────────────────────────

func TestFleetService_Dispatch_TeardownOnOpenSessionFailure(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-9", openErr: context.DeadlineExceeded}
	svc := New(man, agents, nil, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default"})
	require.Error(t, err)
	require.Equal(t, []string{"inst-9"}, man.stops(), "a failed OpenSession must tear the fresh instance back down")
}

func TestFleetService_Dispatch_TeardownOnMissionBindFailure(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-11", openID: "sess-11"}
	// A mission registry that Binds against a nonexistent mission id fails
	// with libdb.ErrNotFound, forcing the same rollback OpenSession failure
	// takes.
	missions := missionservice.New(db)
	svc := New(man, agents, &bindFailingMissions{Service: missions}, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "runner", Intent: "will fail to bind", HITLPolicyName: "default"})
	require.Error(t, err)
	require.Equal(t, []string{"inst-11"}, man.stops(), "a failed mission Bind must tear the fresh instance back down")
}

// bindFailingMissions wraps a real missionservice.Service but makes Bind
// always fail, to exercise Dispatch's rollback path without hand-rolling the
// rest of the Service interface.
type bindFailingMissions struct {
	missionservice.Service
}

func (b *bindFailingMissions) Bind(context.Context, string, string, string) (*missionservice.Mission, error) {
	return nil, context.DeadlineExceeded
}

// ─── Dispatch: cwd envelope ─────────────────────────────────────────────────

func TestFleetService_Dispatch_InvalidCwdRejected(t *testing.T) {
	allowed := t.TempDir()
	roots, err := vfs.NewFactory(allowed)
	require.NoError(t, err)

	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-3", openID: "sess-3"}
	svc := New(man, agents, nil, roots, "", nil)

	_, err = svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default", Cwd: t.TempDir(),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrInvalidParameter)
	require.Empty(t, man.starts(), "rejected before any instance is brought up")
}

// TestFleetService_Dispatch_RelativeCwdRejectedWithoutAllowlist pins the
// TIGHTENING that consolidating cwd resolution onto vfs.ResolveSessionCwd
// brought: every ACP entry point already refused a non-absolute cwd before
// resolving, but this REST path did not. With no allowlist configured its
// hand-rolled predecessor returned a non-empty cwd UNCHANGED, so
// POST /fleet/dispatch {"cwd":"../.."} reached OpenSession with a relative path
// the session paths would have refused. It is now refused here too, before any
// instance is brought up.
func TestFleetService_Dispatch_RelativeCwdRejectedWithoutAllowlist(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-rel", openID: "sess-rel"}
	// nil allowlist — the configuration the hole lived in.
	svc := New(man, agents, nil, nil, "/project/root", nil)

	for _, cwd := range []string{"../..", "relative/path", "."} {
		_, err := svc.Dispatch(ctx, DispatchRequest{
			AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default", Cwd: cwd,
		})
		require.Error(t, err, "cwd %q must be refused", cwd)
		require.ErrorIs(t, err, errdefs.ErrInvalidParameter)
	}
	require.Empty(t, man.starts(), "rejected before any instance is brought up")
	require.Empty(t, man.openSpecs, "no session is ever opened with a relative cwd")
}

// ─── Stop ────────────────────────────────────────────────────────────────────

func TestFleetService_Stop_DelegatesAndIsIdempotent(t *testing.T) {
	man := &fakeManager{}
	svc := New(man, agentregistryservice.New(mustDB(t)), nil, nil, "", nil)

	require.NoError(t, svc.Stop(context.Background(), "inst-1"))
	require.NoError(t, svc.Stop(context.Background(), "inst-1"), "a second Stop is a no-op, per kernel contract")
	require.NoError(t, svc.Stop(context.Background(), "never-existed"), "Stop of an unknown id is a no-op, per kernel contract")
	require.Equal(t, []string{"inst-1", "inst-1", "never-existed"}, man.stops())
}

// ─── Cancel ──────────────────────────────────────────────────────────────────

func TestFleetService_Cancel_WithSessionIDCancelsExactlyThatSession(t *testing.T) {
	man := &fakeManager{}
	svc := New(man, agentregistryservice.New(mustDB(t)), nil, nil, "", nil)

	require.NoError(t, svc.Cancel(context.Background(), "inst-1", "sess-a"))
	require.Equal(t, []cancelCall{{instanceID: "inst-1", sessionID: "sess-a"}}, man.cancels())
}

func TestFleetService_Cancel_EmptySessionIDFansOutOverAllSessions(t *testing.T) {
	man := &fakeManager{
		statuses: map[string]agentinstance.InstanceStatus{
			"inst-1": {ID: "inst-1", SessionIDs: []string{"sess-a", "sess-b", "sess-c"}},
		},
	}
	svc := New(man, agentregistryservice.New(mustDB(t)), nil, nil, "", nil)

	require.NoError(t, svc.Cancel(context.Background(), "inst-1", ""))
	got := man.cancels()
	require.Len(t, got, 3)
	var sessions []string
	for _, c := range got {
		require.Equal(t, "inst-1", c.instanceID)
		sessions = append(sessions, string(c.sessionID))
	}
	require.ElementsMatch(t, []string{"sess-a", "sess-b", "sess-c"}, sessions)
}

func TestFleetService_Cancel_EmptySessionIDNoOpWhenNoSessions(t *testing.T) {
	man := &fakeManager{
		statuses: map[string]agentinstance.InstanceStatus{
			"inst-1": {ID: "inst-1", SessionIDs: nil},
		},
	}
	svc := New(man, agentregistryservice.New(mustDB(t)), nil, nil, "", nil)

	require.NoError(t, svc.Cancel(context.Background(), "inst-1", ""))
	require.Empty(t, man.cancels(), "safe with no turn in flight: zero sessions cancels nothing")
}

func TestFleetService_Cancel_UnknownInstancePropagatesNotFound(t *testing.T) {
	man := &fakeManager{statuses: map[string]agentinstance.InstanceStatus{}}
	svc := New(man, agentregistryservice.New(mustDB(t)), nil, nil, "", nil)

	err := svc.Cancel(context.Background(), "no-such-instance", "")
	require.Error(t, err)
	require.ErrorIs(t, err, agentinstance.ErrNotFound)
}

// ─── Cancel: the fan-out against a REAL kernel ──────────────────────────────
//
// The fakeManager tests above prove the fan-out LOOP; they cannot prove what it
// fans out OVER, because the fake hands back whatever SessionIDs the test set.
// That gap hid a real bug: InstanceStatus.SessionIDs was once derived from the
// kernel's viewer hub, whose per-session state materializes only on a session's
// first delivered update or first attach — so a session that was open but had
// emitted nothing was absent from the list and silently skipped by a
// cancel-everything. On local inference that silent window is the cold model
// load and the long first reasoning pass, i.e. exactly when a cancel matters.
// This test therefore drives the real kernel and a real (hermetic) downstream.

// buildStubAgentBin compiles libacp/cmd/acp-stub-agent — the hermetic in-repo ACP
// agent, no LLM backend — into t.TempDir() and returns its path.
func buildStubAgentBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/libacp/cmd/acp-stub-agent").CombinedOutput()
	require.NoError(t, err, "build acp-stub-agent:\n%s", out)
	return binPath
}

// cancelRecordingManager wraps a REAL agentinstance.Manager and records the Cancel
// calls fleetservice makes, delegating every method (Get included, so SessionIDs is
// the kernel's genuine answer) to the wrapped kernel. It is a spy, not a stub: the
// only thing it fakes is observability.
type cancelRecordingManager struct {
	agentinstance.Manager

	mu      sync.Mutex
	records []cancelCall
}

func (m *cancelRecordingManager) Cancel(instanceID string, sessionID libacp.SessionID) error {
	m.mu.Lock()
	m.records = append(m.records, cancelCall{instanceID: instanceID, sessionID: sessionID})
	m.mu.Unlock()
	return m.Manager.Cancel(instanceID, sessionID)
}

func (m *cancelRecordingManager) cancels() []cancelCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cancelCall(nil), m.records...)
}

// TestFleetService_Cancel_EmptySessionIDReachesSilentSession is the regression test
// for that bug, driven end-to-end: dispatch (whose intent-driven first turn resolves
// immediately and touches nothing this test cares about — see below), then assert a
// session-less Cancel still reaches the session even though nobody ever attached a
// viewer to it.
func TestFleetService_Cancel_EmptySessionIDReachesSilentSession(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)

	agent := &runtimetypes.Agent{Name: "silent-runner", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   buildStubAgentBin(t),
	}))
	require.NoError(t, agents.Create(ctx, agent))

	kernel := agentinstance.New(agents)
	t.Cleanup(func() { _ = kernel.Close() })
	man := &cancelRecordingManager{Manager: kernel}

	// Every dispatch is a mission now, so this end-to-end kernel test needs a
	// real mission registry too, even though the mission record itself is
	// incidental to what this test proves (the Cancel fan-out).
	missions := missionservice.New(db)
	svc := New(man, agents, missions, nil, t.TempDir(), libtracker.NoopTracker{})

	// The stub agent's first turn resolves quickly and leaves the session open
	// and quiet, with Dispatch attaching no viewer. This is the exact shape of
	// the black hole adopt/cancel exist for.
	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "silent-runner", Intent: "do the thing", HITLPolicyName: "default",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.SessionID)

	// The stub unit files no mission fact, so the runtime nudges once and then
	// files a blocker. Wait for that to settle: it leaves the session OPEN but
	// IDLE (prompt turns do not close a session), which is exactly the quiescent
	// black hole this test's cancel assertions want — and it keeps the detached
	// goroutine from racing t.Cleanup's teardown.
	require.Eventually(t, func() bool {
		reps, lerr := missions.ListReports(ctx, result.MissionID, 5)
		return lerr == nil && len(reps) > 0
	}, 20*time.Second, 50*time.Millisecond, "the unattended unit's nudge-then-blocker never settled")

	st, err := svc.Get(ctx, result.InstanceID)
	require.NoError(t, err)
	require.Equal(t, []string{result.SessionID}, st.SessionIDs,
		"an open-but-silent session must be visible on the fleet board")
	require.Zero(t, st.Viewers, "nothing is watching it — the condition that used to hide it")

	// THE REGRESSION: a session-less Cancel must reach that session, not skip it.
	require.NoError(t, svc.Cancel(ctx, result.InstanceID, ""))
	require.Equal(t,
		[]cancelCall{{instanceID: result.InstanceID, sessionID: libacp.SessionID(result.SessionID)}},
		man.cancels(),
		"cancel-everything must cancel the silent session")

	// Closing the session removes it from the fan-out set, so a second Cancel is a
	// genuine no-op rather than an error — the negative half of the same contract.
	require.NoError(t, kernel.CloseSession(result.InstanceID, libacp.SessionID(result.SessionID)))
	require.NoError(t, svc.Cancel(ctx, result.InstanceID, ""))
	require.Len(t, man.cancels(), 1, "a closed session is not cancelled again")
}

// ─── small local helpers (kept out of the setup section above since they are
//     one-liners used by exactly the Stop/Cancel tests, which don't need a
//     real agent registered) ───────────────────────────────────────────────

func mustDB(t *testing.T) libdb.DBManager {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleetservice-unused.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ─── the supervision edge ──────────────────────────────────────────────────

// Dispatch records WHO fired the mission when the caller knows, and leaves it
// empty when it does not. SessionID/InstanceID say what the mission SPAWNED;
// ParentSessionID says who fired it — the edge the record could not express
// before, and the one report routing needs.
func TestFleetService_Dispatch_RecordsParentSession(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-9", openID: "sess-9"}
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{})

	fired, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:       "runner",
		Intent:          "investigate the failure",
		HITLPolicyName:  "default",
		ParentSessionID: "upstream-session-3",
	})
	require.NoError(t, err)

	m, err := missions.Get(ctx, fired.MissionID)
	require.NoError(t, err)
	require.Equal(t, "upstream-session-3", m.ParentSessionID)
	require.Equal(t, "sess-9", m.SessionID, "the spawned session is a different fact from the parent")
	waitMissionSettled(t, missions, fired.MissionID)

	// A second unit, fired without a parent, on its own fakeManager.
	man2 := &fakeManager{startID: "inst-10", openID: "sess-10"}
	svc2 := New(man2, agents, missions, nil, "/project/root", libtracker.NoopTracker{})

	direct, err := svc2.Dispatch(ctx, DispatchRequest{
		AgentName:      "runner",
		Intent:         "fired by an operator",
		HITLPolicyName: "default",
	})
	require.NoError(t, err)

	m2, err := missions.Get(ctx, direct.MissionID)
	require.NoError(t, err)
	require.Empty(t, m2.ParentSessionID, "an operator-fired mission has no parent session")
	waitMissionSettled(t, missions, direct.MissionID)
}

// ─── the unattended-turn cure: heartbeat, one nudge, then a runtime blocker ──
//
// These are the FAST (no-subprocess) siblings of the misbehaving-fixture
// acceptance e2e (e2e_unattended_nudge_test.go). The pure decision and the
// blocker text are pinned as TestUnit_*; the loop's shape is driven through the
// fakeManager against a real (sqlite) mission store.

// TestUnit_missionShowsUnitReached is the pure decision at the heart of the nudge
// loop, exhaustive over the facts it keys on.
func TestUnit_missionShowsUnitReached(t *testing.T) {
	open := &missionservice.Mission{Status: missionservice.StatusOpen}
	require.False(t, missionShowsUnitReached(open, 0), "a bare open mission reached no one")
	require.True(t, missionShowsUnitReached(open, 1), "a filed report is the unit reaching the operator")

	planned := &missionservice.Mission{Status: missionservice.StatusOpen, Plan: missionservice.Plan{Revision: 1}}
	require.True(t, missionShowsUnitReached(planned, 0), "a plan revision is a mission-tool fact")

	for _, term := range []missionservice.Status{
		missionservice.StatusLanded, missionservice.StatusDerailed,
		missionservice.StatusStuck, missionservice.StatusAbandoned,
	} {
		require.Truef(t, missionShowsUnitReached(&missionservice.Mission{Status: term}, 0),
			"a terminal verdict (%s) is the unit finishing its mission", term)
	}

	require.False(t, missionShowsUnitReached(nil, 0), "no mission and no report is not-reached")
	require.True(t, missionShowsUnitReached(nil, 2), "reports alone suffice even if the mission read failed")
}

// TestUnit_silentTurnBlocker pins the two shapes of the runtime-filed blocker and
// the single-line-summary invariant missionservice.AddReport validation requires.
func TestUnit_silentTurnBlocker(t *testing.T) {
	// With recoverable last words: the summary QUOTES them, single-lined; the
	// detail keeps the full text verbatim and points at the session.
	sum, det := silentTurnBlocker("I need to know which\nbranch to target.", "sess-1")
	require.NotContains(t, sum, "\n", "a report summary must be a single line")
	require.Contains(t, sum, "which branch to target", "the summary carries the unit's own words")
	require.Contains(t, det, "I need to know which\nbranch to target.", "the detail keeps the full text verbatim")
	require.Contains(t, det, "sess-1")

	// Without recoverable text: a clear generic pointing at the session.
	sum, det = silentTurnBlocker("   ", "sess-2")
	require.NotEmpty(t, sum)
	require.NotContains(t, sum, "\n")
	require.Contains(t, sum, "sess-2")
	require.Contains(t, det, "sess-2")

	// A pathological long single line still yields a bounded single-line summary.
	sum, _ = silentTurnBlocker(strings.Repeat("x", 5000), "sess-3")
	require.NotContains(t, sum, "\n")
	require.LessOrEqual(t, len([]rune(sum)), 241, "the excerpt is truncated (max runes + ellipsis)")
}

// TestFleetService_Dispatch_BareUnitNudgedOnceThenBlocked drives the cure with a
// fake kernel whose unit only ever ends its turn (never files a mission fact):
// the runtime stamps liveness, nudges EXACTLY once, and — still mute — files a
// blocker itself, with no third prompt and the mission left OPEN (blocked, not
// terminal).
func TestFleetService_Dispatch_BareUnitNudgedOnceThenBlocked(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-bare", openID: "sess-bare", agentText: "which branch should I target?"}
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{})

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "migrate the module", HITLPolicyName: "default",
	})
	require.NoError(t, err)

	// The runtime files exactly one blocker once the unit is mute across both turns.
	require.Eventually(t, func() bool {
		reps, lerr := missions.ListReports(ctx, res.MissionID, 5)
		return lerr == nil && len(reps) == 1
	}, 5*time.Second, 20*time.Millisecond, "the runtime should file one blocker for a mute unit")

	reps, err := missions.ListReports(ctx, res.MissionID, 5)
	require.NoError(t, err)
	require.Len(t, reps, 1)
	require.Equal(t, missionservice.ReportKindBlocker, reps[0].Kind)
	require.Contains(t, reps[0].Summary, "which branch should I target",
		"the runtime-filed blocker quotes the unit's last words")

	// Exactly two prompts: the intent turn and ONE nudge. No third, ever.
	require.Equal(t, 2, man.prompts(), "one intent turn + exactly one nudge, hard-capped")

	// Turn 1 = [preamble, clean intent]; turn 2 = [nudge]. The intent is stored clean.
	blocks := man.promptCallBlocks()
	require.Len(t, blocks, 2)
	require.Len(t, blocks[0], 2, "the first turn is the preamble ahead of the intent")
	preText, _ := libacp.FlattenContent(blocks[0][:1])
	require.Equal(t, missionPreamble, preText)
	intentText, _ := libacp.FlattenContent(blocks[0][1:])
	require.Equal(t, "migrate the module", intentText, "the intent runs clean; the preamble is wire-only")
	nudgeText, _ := libacp.FlattenContent(blocks[1])
	require.Equal(t, missionNudge, nudgeText)

	// Liveness got stamped (turn completion is liveness), and the mission is NOT
	// terminal — it is blocked, not done.
	m, err := missions.Get(ctx, res.MissionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "every completed turn stamps liveness")
	require.Equal(t, missionservice.StatusOpen, m.Status, "a nudged-then-blocked mission stays open, not terminal")
	require.Equal(t, "migrate the module", m.Intent, "the preamble never persisted as the intent")
}

// TestFleetService_Dispatch_ReportingUnitGetsNoNudge is the happy-path guard: a
// unit that files a mission report on its first turn is NOT nudged — the runtime
// sends exactly one prompt and files no blocker of its own.
func TestFleetService_Dispatch_ReportingUnitGetsNoNudge(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-rep", openID: "sess-rep"}
	// The "unit" files a report on its first turn, over the same store a real
	// dispatched unit writes to — so missionReached() is true and no nudge follows.
	man.onPrompt = func(call int) {
		if call != 1 {
			return
		}
		if ms, _ := missions.List(context.Background(), nil, 10); len(ms) == 1 {
			_ = missions.AddReport(context.Background(), ms[0].ID, &missionservice.Report{
				Kind: missionservice.ReportKindResult, Summary: "did the thing",
			})
		}
	}
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{})

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default",
	})
	require.NoError(t, err)

	// The unit's single report lands and NOTHING else is added.
	require.Eventually(t, func() bool {
		reps, lerr := missions.ListReports(ctx, res.MissionID, 5)
		return lerr == nil && len(reps) == 1
	}, 5*time.Second, 20*time.Millisecond)

	// Give any (erroneous) nudge a chance to happen, then prove it did not.
	require.Never(t, func() bool {
		return man.prompts() > 1
	}, 300*time.Millisecond, 50*time.Millisecond, "a unit that reported must not be nudged")

	require.Equal(t, 1, man.prompts(), "exactly one turn: the intent, no nudge")
	reps, err := missions.ListReports(ctx, res.MissionID, 5)
	require.NoError(t, err)
	require.Len(t, reps, 1, "only the unit's own report — no runtime blocker")
	require.Equal(t, missionservice.ReportKindResult, reps[0].Kind)
}

// TestFleetService_Dispatch_EmptyCwdResolvesToAllowlistDefault pins that an absent
// cwd resolves to the workspace ALLOWLIST default (the effective root),
// authoritatively — NOT to a divergent projectRoot. This is the guard that keeps
// the traced footgun (a stray $HOME projectRoot leaking as a dispatched unit's
// cwd) from ever reappearing: when a Factory is configured, its default wins.
func TestFleetService_Dispatch_EmptyCwdResolvesToAllowlistDefault(t *testing.T) {
	allowed := t.TempDir()
	roots, err := vfs.NewFactory(allowed)
	require.NoError(t, err)
	resolvedAllowed, err := vfs.ResolveRoot(allowed) // the symlink-resolved form the Factory stores
	require.NoError(t, err)

	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-cwd", openID: "sess-cwd"}
	// projectRoot deliberately DIVERGES from the allowlist default — the shape of
	// the footgun. It must resolve to the allowlist, never to the stray fallback.
	svc := New(man, agents, missions, roots, "/some/other/home", libtracker.NoopTracker{})

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default",
		// Cwd omitted.
	})
	require.NoError(t, err)

	require.Len(t, man.openSpecs, 1)
	require.Equal(t, resolvedAllowed, man.openSpecs[0].Cwd,
		"an absent cwd resolves to the allowlist default, not the divergent projectRoot")
	require.NotEqual(t, "/some/other/home", man.openSpecs[0].Cwd)

	waitMissionSettled(t, missions, res.MissionID)
}
