package fleetservice

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

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
	promptBlocks       []libacp.ContentBlock   // blocks of the most recent prompt
	promptBlocksByCall [][]libacp.ContentBlock // blocks of every prompt, in order
	// onPrompt, when set, runs on each Prompt call (1-based) before it
	// returns. Runs under no lock.
	onPrompt func(call int)
	// agentText is what SessionAgentText returns for any (instance, session).
	agentText string

	stopCalls []string

	cancelErr   error
	cancelCalls []cancelCall

	statuses map[string]agentinstance.InstanceStatus

	// listStates configures List: each entry becomes one live instance in
	// that state. listErr, when set, makes List fail instead.
	listStates []string
	listErr    error
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

// StartResolved records the spawn under the resolved record's name, so every
// existing starts() assertion keeps reading the same way.
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

// SessionAgentText satisfies fleetservice's optional sessionTextReader
// capability, returning the configured agentText for any (instance, session).
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	if len(m.listStates) == 0 {
		return nil, nil
	}
	instances := make([]agentinstance.InstanceStatus, len(m.listStates))
	for i, state := range m.listStates {
		instances[i] = agentinstance.InstanceStatus{ID: fmt.Sprintf("listed-%d", i), State: state}
	}
	return []agentinstance.FleetEntry{{AgentName: "runner", Instances: instances}}, nil
}

func (m *fakeManager) setListStates(states ...string) {
	m.mu.Lock()
	m.listStates = states
	m.mu.Unlock()
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

func setupRegistryDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleetservice.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

func registerAgent(t *testing.T, ctx context.Context, agents agentregistryservice.Service, name string, enabled bool) {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: enabled}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "/bin/true",
	}))
	require.NoError(t, agents.Create(ctx, agent))
}

func waitMissionSettled(t *testing.T, missions missionservice.Service, missionID string) {
	t.Helper()
	// waits for the runtime blocker (the goroutine's last write) so t.Cleanup's db close cannot race it.
	require.Eventually(t, func() bool {
		reps, err := missions.ListReports(context.Background(), missionID, 5)
		return err == nil && len(reps) > 0
	}, 5*time.Second, 20*time.Millisecond, "dispatch goroutine never settled (no runtime blocker filed)")
}

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

	man := &fakeManager{startID: "inst-1", openID: "sess-1"}
	svc := New(man, agents, nil, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "ghost", Intent: "do the thing", HITLPolicyName: "default"})
	require.Error(t, err)
	require.ErrorIs(t, err, libdb.ErrNotFound)
	require.Empty(t, man.starts())
}

// TestFleetService_Dispatch_HappyPath: an instance comes up, a session opens,
// the mission is created bound to both fresh ids, and the intent runs as the
// unit's first turn.
func TestFleetService_Dispatch_HappyPath(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-7", openID: "sess-7"}
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

	require.Len(t, man.openSpecs, 1)
	require.Equal(t, "/project/root", man.openSpecs[0].Cwd)

	m, err := missions.Get(ctx, result.MissionID)
	require.NoError(t, err)
	require.Equal(t, "ship the board", m.Intent, "the intent is stored CLEAN — the preamble is wire-only")
	require.Equal(t, "runner", m.AgentName)
	require.Equal(t, "default", m.HITLPolicyName, "the mission carries the request's envelope")
	require.Equal(t, "sess-7", m.SessionID, "bound to the session Dispatch just opened")
	require.Equal(t, "inst-7", m.InstanceID, "bound to the instance Dispatch just started")

	require.Eventually(t, func() bool {
		reps, _ := missions.ListReports(ctx, result.MissionID, 5)
		return len(reps) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, 1, man.prompts(), "a unit that reported is not nudged")
	require.Empty(t, man.stops(), "a successful dispatch must never stop the instance it just brought up")

	blocks := man.promptCallBlocks()
	require.Len(t, blocks, 1)
	require.Len(t, blocks[0], 2, "the first turn is the preamble ahead of the intent")
	intentText, _ := libacp.FlattenContent(blocks[0][1:])
	require.Equal(t, "ship the board", intentText, "the intent runs as the unit's first turn")
}

// TestFleetService_Dispatch_ResolvesTheAgentExactlyOnce: Dispatch resolves the
// agent once and hands that record to the kernel, closing the spawn-path
// TOCTOU window.
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
	missions := missionservice.New(db)
	svc := New(man, agents, &bindFailingMissions{Service: missions}, nil, "/project/root", nil)

	_, err := svc.Dispatch(ctx, DispatchRequest{AgentName: "runner", Intent: "will fail to bind", HITLPolicyName: "default"})
	require.Error(t, err)
	require.Equal(t, []string{"inst-11"}, man.stops(), "a failed mission Bind must tear the fresh instance back down")
}

type bindFailingMissions struct {
	missionservice.Service
}

func (b *bindFailingMissions) Bind(context.Context, string, string, string) (*missionservice.Mission, error) {
	return nil, context.DeadlineExceeded
}

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

func TestFleetService_Dispatch_RelativeCwdRejectedWithoutAllowlist(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)

	man := &fakeManager{startID: "inst-rel", openID: "sess-rel"}
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

func TestFleetService_Stop_DelegatesAndIsIdempotent(t *testing.T) {
	man := &fakeManager{}
	svc := New(man, agentregistryservice.New(mustDB(t)), nil, nil, "", nil)

	require.NoError(t, svc.Stop(context.Background(), "inst-1"))
	require.NoError(t, svc.Stop(context.Background(), "inst-1"), "a second Stop is a no-op, per kernel contract")
	require.NoError(t, svc.Stop(context.Background(), "never-existed"), "Stop of an unknown id is a no-op, per kernel contract")
	require.Equal(t, []string{"inst-1", "inst-1", "never-existed"}, man.stops())
}

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

func buildStubAgentBin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("external agent spawn runs through the sandbox, which is Landlock-based and Linux-only")
	}
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/libacp/cmd/acp-stub-agent").CombinedOutput()
	require.NoError(t, err, "build acp-stub-agent:\n%s", out)
	return binPath
}

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

// TestFleetService_Cancel_EmptySessionIDReachesSilentSession: a session-less
// Cancel reaches a session even though nobody ever attached a viewer to it.
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

	missions := missionservice.New(db)
	svc := New(man, agents, missions, nil, t.TempDir(), libtracker.NoopTracker{})

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "silent-runner", Intent: "do the thing", HITLPolicyName: "default",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.SessionID)

	// settle before assertions: the detached goroutine would otherwise race them.
	require.Eventually(t, func() bool {
		reps, lerr := missions.ListReports(ctx, result.MissionID, 5)
		return lerr == nil && len(reps) > 0
	}, 20*time.Second, 50*time.Millisecond, "the unattended unit's nudge-then-blocker never settled")

	st, err := svc.Get(ctx, result.InstanceID)
	require.NoError(t, err)
	require.Equal(t, []string{result.SessionID}, st.SessionIDs,
		"an open-but-silent session must be visible on the fleet board")
	require.Zero(t, st.Viewers, "nothing is watching it — the condition that used to hide it")

	require.NoError(t, svc.Cancel(ctx, result.InstanceID, ""))
	require.Equal(t,
		[]cancelCall{{instanceID: result.InstanceID, sessionID: libacp.SessionID(result.SessionID)}},
		man.cancels(),
		"cancel-everything must cancel the silent session")

	require.NoError(t, kernel.CloseSession(result.InstanceID, libacp.SessionID(result.SessionID)))
	require.NoError(t, svc.Cancel(ctx, result.InstanceID, ""))
	require.Len(t, man.cancels(), 1, "a closed session is not cancelled again")
}

func mustDB(t *testing.T) libdb.DBManager {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleetservice-unused.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestFleetService_Dispatch_RecordsParentSession: Dispatch records who fired
// the mission when known, and leaves ParentSessionID empty otherwise.
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

// TestUnit_missionShowsUnitReached: exhaustive over the facts the nudge loop
// keys on.
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

// TestUnit_silentTurnBlocker: the two shapes of the runtime-filed blocker,
// and the single-line-summary invariant missionservice.AddReport requires.
func TestUnit_silentTurnBlocker(t *testing.T) {
	sum, det := silentTurnBlocker("I need to know which\nbranch to target.", "sess-1")
	require.NotContains(t, sum, "\n", "a report summary must be a single line")
	require.Contains(t, sum, "which branch to target", "the summary carries the unit's own words")
	require.Contains(t, det, "I need to know which\nbranch to target.", "the detail keeps the full text verbatim")
	require.Contains(t, det, "sess-1")

	sum, det = silentTurnBlocker("   ", "sess-2")
	require.NotEmpty(t, sum)
	require.NotContains(t, sum, "\n")
	require.Contains(t, sum, "sess-2")
	require.Contains(t, det, "sess-2")

	sum, _ = silentTurnBlocker(strings.Repeat("x", 5000), "sess-3")
	require.NotContains(t, sum, "\n")
	require.LessOrEqual(t, len([]rune(sum)), 241, "the excerpt is truncated (max runes + ellipsis)")
}

// TestFleetService_Dispatch_BareUnitNudgedOnceThenBlocked: nudged once, then
// a runtime blocker, mission left open.
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

	require.Equal(t, 2, man.prompts(), "one intent turn + exactly one nudge, hard-capped")

	blocks := man.promptCallBlocks()
	require.Len(t, blocks, 2)
	require.Len(t, blocks[0], 2, "the first turn is the preamble ahead of the intent")
	preText, _ := libacp.FlattenContent(blocks[0][:1])
	require.Equal(t, missionPreamble, preText)
	intentText, _ := libacp.FlattenContent(blocks[0][1:])
	require.Equal(t, "migrate the module", intentText, "the intent runs clean; the preamble is wire-only")
	nudgeText, _ := libacp.FlattenContent(blocks[1])
	require.Equal(t, missionNudge, nudgeText)

	m, err := missions.Get(ctx, res.MissionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "every completed turn stamps liveness")
	require.Equal(t, missionservice.StatusOpen, m.Status, "a nudged-then-blocked mission stays open, not terminal")
	require.Equal(t, "migrate the module", m.Intent, "the preamble never persisted as the intent")
}

func TestFleetService_Dispatch_ReportingUnitGetsNoNudge(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-rep", openID: "sess-rep"}
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

	require.Eventually(t, func() bool {
		reps, lerr := missions.ListReports(ctx, res.MissionID, 5)
		return lerr == nil && len(reps) == 1
	}, 5*time.Second, 20*time.Millisecond)

	require.Never(t, func() bool {
		return man.prompts() > 1
	}, 300*time.Millisecond, 50*time.Millisecond, "a unit that reported must not be nudged")

	require.Equal(t, 1, man.prompts(), "exactly one turn: the intent, no nudge")
	reps, err := missions.ListReports(ctx, res.MissionID, 5)
	require.NoError(t, err)
	require.Len(t, reps, 1, "only the unit's own report — no runtime blocker")
	require.Equal(t, missionservice.ReportKindResult, reps[0].Kind)
}

// TestFleetService_Dispatch_EmptyCwdResolvesToAllowlistDefault: an absent cwd
// resolves to the workspace allowlist default, never to a divergent
// projectRoot.
func TestFleetService_Dispatch_EmptyCwdResolvesToAllowlistDefault(t *testing.T) {
	allowed := t.TempDir()
	roots, err := vfs.NewFactory(allowed)
	require.NoError(t, err)
	resolvedAllowed, err := vfs.ResolveRoot(allowed)
	require.NoError(t, err)

	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-cwd", openID: "sess-cwd"}
	svc := New(man, agents, missions, roots, "/some/other/home", libtracker.NoopTracker{})

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default",
	})
	require.NoError(t, err)

	require.Len(t, man.openSpecs, 1)
	require.Equal(t, resolvedAllowed, man.openSpecs[0].Cwd,
		"an absent cwd resolves to the allowlist default, not the divergent projectRoot")
	require.NotEqual(t, "/some/other/home", man.openSpecs[0].Cwd)

	waitMissionSettled(t, missions, res.MissionID)
}

func TestUnit_DriveLoop_TurnErrorFinishesDerailedAndReapsTheUnit(t *testing.T) {
	mgr := &fakeManager{openID: "sess-1", promptErr: fmt.Errorf("backend refused the turn")}
	svc, missions, run := driveFixture(t, hitlservice.ComputeBounds{}, mgr)

	svc.driveUnattendedMission(context.Background(), run)

	require.Equal(t, 1, mgr.prompts(), "a failed turn is neither retried nor nudged")
	require.Equal(t, []string{"inst-1"}, mgr.stops(),
		"the unit nobody drives is reaped rather than left holding a seat against fleet-max-parallel")

	got, err := missions.Get(context.Background(), run.missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusDerailed, got.Status, "a dead turn is a post-mortem, not a judgement call")
	require.Contains(t, got.StatusReason, turnErrorBlockerLead)
	require.Contains(t, got.StatusReason, "backend refused the turn", "the reason quotes what actually failed")
	require.Nil(t, got.LastHeartbeat,
		"a failed turn stamps no liveness: refreshing the stale clock at the moment the turn died would hide the mission from the sweep")

	reps, err := missions.ListReports(context.Background(), run.missionID, 5)
	require.NoError(t, err)
	require.Len(t, reps, 1, "the runtime files the blocker itself; a mute-path blocker never runs on this path")
	require.Equal(t, missionservice.ReportKindBlocker, reps[0].Kind)
	require.Contains(t, reps[0].Detail, "backend refused the turn")
}

// TestUnit_DriveLoop_NudgeTurnErrorTakesTheSamePath pins the second prompt
// site, so the failure path cannot be wired on one call and forgotten on the
// other.
func TestUnit_DriveLoop_NudgeTurnErrorTakesTheSamePath(t *testing.T) {
	mgr := &fakeManager{openID: "sess-1", agentText: "still thinking"}
	mgr.onPrompt = func(call int) {
		if call >= 2 {
			mgr.promptErr = fmt.Errorf("backend died mid-mission")
		}
	}
	svc, missions, run := driveFixture(t, hitlservice.ComputeBounds{}, mgr)

	svc.driveUnattendedMission(context.Background(), run)

	require.Equal(t, 2, mgr.prompts(), "one intent turn plus the one nudge, still hard-capped")
	require.Equal(t, []string{"inst-1"}, mgr.stops())

	got, err := missions.Get(context.Background(), run.missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusDerailed, got.Status)
	require.Contains(t, got.StatusReason, "backend died mid-mission")
	require.NotNil(t, got.LastHeartbeat, "turn 1 completed, so its liveness stands; only the failed turn stamps nothing")
}

// TestUnit_DriveLoop_TurnErrorOnAnAlreadyStoppedMissionOnlyReapsTheUnit pins the common case: an operator's `mission stop` reaps the subprocess (itself how the in-flight turn errored), and that mission owns its own verdict.
func TestUnit_DriveLoop_TurnErrorOnAnAlreadyStoppedMissionOnlyReapsTheUnit(t *testing.T) {
	mgr := &fakeManager{openID: "sess-1"}
	svc, missions, run := driveFixture(t, hitlservice.ComputeBounds{}, mgr)
	mgr.onPrompt = func(int) {
		_, err := missions.Finish(context.Background(), run.missionID, missionservice.StatusAbandoned, "stopped by operator")
		require.NoError(t, err)
		mgr.promptErr = fmt.Errorf("session closed")
	}

	svc.driveUnattendedMission(context.Background(), run)

	require.Equal(t, []string{"inst-1"}, mgr.stops(), "the unit is reaped either way")

	got, err := missions.Get(context.Background(), run.missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusAbandoned, got.Status, "the operator's verdict is not relabelled derailed")
	require.Equal(t, "stopped by operator", got.StatusReason)

	reps, err := missions.ListReports(context.Background(), run.missionID, 5)
	require.NoError(t, err)
	require.Empty(t, reps, "and a mission already at rest gets no second story about why it ended")
}

// TestFleetService_Dispatch_TurnErrorSettlesTheMissionEndToEnd walks the same
// failure through Dispatch's detached goroutine, since that is where a leaked
// seat would actually accumulate.
func TestFleetService_Dispatch_TurnErrorSettlesTheMissionEndToEnd(t *testing.T) {
	ctx, db := setupRegistryDB(t)
	agents := agentregistryservice.New(db)
	registerAgent(t, ctx, agents, "runner", true)
	missions := missionservice.New(db)

	man := &fakeManager{startID: "inst-err", openID: "sess-err", promptErr: fmt.Errorf("no backend configured")}
	svc := New(man, agents, missions, nil, "/project/root", libtracker.NoopTracker{})

	res, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName: "runner", Intent: "do the thing", HITLPolicyName: "default",
	})
	require.NoError(t, err, "a turn that fails later never fails the dispatch itself")

	require.Eventually(t, func() bool {
		m, gerr := missions.Get(ctx, res.MissionID)
		return gerr == nil && m.Status == missionservice.StatusDerailed
	}, 5*time.Second, 20*time.Millisecond, "the drive goroutine must settle the mission, not return silently")

	require.Eventually(t, func() bool {
		return len(man.stops()) == 1
	}, 5*time.Second, 20*time.Millisecond, "and reap the unit it can no longer drive")
	require.Equal(t, []string{"inst-err"}, man.stops())
}
