package agentinstance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// buildStubAgent compiles the hermetic, in-memory acp-stub-agent fixture into
// t.TempDir() and returns its path, for tests to spawn as a real ACP agent
// subprocess with no network and no model. Every caller spawns it through the
// sandbox, which is Landlock-based and Linux-only (see
// internal/libsandbox/isolation_other.go) — off Linux the spawn always fails
// with ErrIsolation before the binary is even exec'd, so there is nothing
// meaningful left to test.
func buildStubAgent(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("external agent spawn runs through the sandbox, which is Landlock-based and Linux-only")
	}
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/libacp/cmd/acp-stub-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build acp-stub-agent: %v\n%s", err, out)
	}
	return binPath
}

func setupRegistry(t *testing.T) (context.Context, libdb.DBManager, agentregistryservice.Service) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "agentinstance.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db, agentregistryservice.New(db)
}

// registerExternal declares an external_acp agent named name that spawns command
// with args, via the registry service (the normal path).
func registerExternal(t *testing.T, ctx context.Context, svc agentregistryservice.Service, name, command string, args ...string) *runtimetypes.Agent {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   command,
		Args:      args,
	}))
	require.NoError(t, svc.Create(ctx, agent))
	return agent
}

// registerExternalEnv is registerExternal with a subprocess environment, used to flip
// the acp-stub-agent's opt-in scenario flags (ACP_STUB_ADVERTISE_*, ACP_STUB_USE_TERMINAL).
func registerExternalEnv(t *testing.T, ctx context.Context, svc agentregistryservice.Service, name, command string, env map[string]string, args ...string) *runtimetypes.Agent {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   command,
		Args:      args,
		Env:       env,
	}))
	require.NoError(t, svc.Create(ctx, agent))
	return agent
}

// registerChain declares a chain-kind agent running chainPath, through the
// normal registry path.
func registerChain(t *testing.T, ctx context.Context, svc agentregistryservice.Service, name, chainPath string) *runtimetypes.Agent {
	t.Helper()
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetChainConfig(runtimetypes.ChainConfig{Path: chainPath, ChainID: "fixture-chain"}))
	require.NoError(t, svc.Create(ctx, agent))
	return agent
}

// instanceOf reaches into the Manager's registry (white-box) to fetch the
// live instance for id.
func instanceOf(t *testing.T, m Manager, id string) *instance {
	t.Helper()
	impl := m.(*manager)
	impl.mu.Lock()
	defer impl.mu.Unlock()
	inst, ok := impl.instances[id]
	require.True(t, ok, "instance %q not in registry", id)
	return inst
}

// currentHandle reads an instance's live handle under its lock (white-box),
// so a test can close it out-of-band to simulate a crash.
func currentHandle(inst *instance) *agenthost.Handle {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.handle
}

func requireConnClosed(t *testing.T, h *agenthost.Handle) {
	t.Helper()
	select {
	case <-h.Conn.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("agent connection did not close after teardown (subprocess leak)")
	}
}

// openSession drives a session through Manager.OpenSession and returns the
// downstream session id that viewers Attach to.
func openSession(t *testing.T, mgr Manager, id string) libacp.SessionID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, err := mgr.OpenSession(ctx, id, SessionSpec{Cwd: t.TempDir()})
	require.NoError(t, err)
	require.NotEmpty(t, sid)
	return sid
}

// promptText drives one prompt turn through the kernel API and returns its stop reason.
func promptText(t *testing.T, mgr Manager, id string, sid libacp.SessionID, text string) libacp.StopReason {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reason, err := mgr.Prompt(ctx, id, sid, []libacp.ContentBlock{libacp.NewTextContent(text)})
	require.NoError(t, err)
	return reason
}

// mockViewer is a test Viewer that records delivered updates and permission
// requests, answering permission with a preset outcome (default: cancelled).
type mockViewer struct {
	id       string
	permKind libacp.PermissionOutcomeKind
	optionID string

	mu        sync.Mutex
	updates   []libacp.SessionNotification
	permCalls int
}

func newMockViewer(id string) *mockViewer {
	return &mockViewer{id: id, permKind: libacp.PermissionOutcomeCancelled}
}

func (v *mockViewer) ID() string { return v.id }

func (v *mockViewer) Deliver(_ context.Context, n libacp.SessionNotification) error {
	v.mu.Lock()
	v.updates = append(v.updates, n)
	v.mu.Unlock()
	return nil
}

func (v *mockViewer) RequestPermission(_ context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	v.mu.Lock()
	v.permCalls++
	v.mu.Unlock()
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: v.permKind, OptionID: v.optionID},
	}, nil
}

func (v *mockViewer) updateCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.updates)
}

func (v *mockViewer) permCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.permCalls
}

// viewerReported reports whether any agent_message_chunk delivered to v
// contains substr.
func viewerReported(v *mockViewer, substr string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, n := range v.updates {
		if n.Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk {
			if c := n.Update.Content; c != nil && strings.Contains(c.Text, substr) {
				return true
			}
		}
	}
	return false
}

// mockTerminalViewer is a Viewer that also implements TerminalServer,
// recording the create call and returning a canned output.
type mockTerminalViewer struct {
	id string

	mu      sync.Mutex
	updates []libacp.SessionNotification
	creates int
}

func newMockTerminalViewer(id string) *mockTerminalViewer { return &mockTerminalViewer{id: id} }

func (v *mockTerminalViewer) ID() string { return v.id }

func (v *mockTerminalViewer) Deliver(_ context.Context, n libacp.SessionNotification) error {
	v.mu.Lock()
	v.updates = append(v.updates, n)
	v.mu.Unlock()
	return nil
}

func (v *mockTerminalViewer) RequestPermission(_ context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	return libacp.RequestPermissionResponse{Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled}}, nil
}

func (v *mockTerminalViewer) CreateTerminal(_ context.Context, _ libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	v.mu.Lock()
	v.creates++
	v.mu.Unlock()
	return libacp.CreateTerminalResponse{TerminalID: "mock-term-1"}, nil
}

func (v *mockTerminalViewer) TerminalOutput(_ context.Context, _ libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	return libacp.TerminalOutputResponse{Output: "MOCK-TERMINAL-OUTPUT"}, nil
}

func (v *mockTerminalViewer) WaitForTerminalExit(_ context.Context, _ libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	code := 0
	return libacp.WaitForTerminalExitResponse{ExitCode: &code}, nil
}

func (v *mockTerminalViewer) KillTerminal(_ context.Context, _ libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	return libacp.KillTerminalResponse{}, nil
}

func (v *mockTerminalViewer) ReleaseTerminal(_ context.Context, _ libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	return libacp.ReleaseTerminalResponse{}, nil
}

func (v *mockTerminalViewer) createCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.creates
}

func (v *mockTerminalViewer) lastMessage() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var last string
	for _, n := range v.updates {
		if n.Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk {
			if c := n.Update.Content; c != nil {
				last = c.Text
			}
		}
	}
	return last
}

// blockingViewer is a controller whose RequestPermission blocks until its
// context is cancelled, signaling on arrived once the request reaches it.
type blockingViewer struct {
	id      string
	arrived chan struct{}
	once    sync.Once
}

func newBlockingViewer(id string) *blockingViewer {
	return &blockingViewer{id: id, arrived: make(chan struct{})}
}

func (v *blockingViewer) ID() string { return v.id }

func (v *blockingViewer) Deliver(_ context.Context, _ libacp.SessionNotification) error { return nil }

func (v *blockingViewer) RequestPermission(ctx context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	v.once.Do(func() { close(v.arrived) })
	<-ctx.Done()
	return libacp.RequestPermissionResponse{Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled}}, nil
}

func TestManager_External_StartGetStop(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	require.NotEmpty(t, id)

	st, err := mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, st.State)
	require.Equal(t, runtimetypes.AgentKindExternalACP, st.Kind)
	require.Equal(t, "ext-agent", st.AgentName)
	require.Equal(t, 0, st.Sessions)
	require.Equal(t, 0, st.Viewers)
	require.False(t, st.StartedAt.IsZero())

	handle := currentHandle(instanceOf(t, mgr, id))
	require.NotNil(t, handle)

	require.NoError(t, mgr.Stop(id))
	requireConnClosed(t, handle)

	_, err = mgr.Get(id)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestManager_Chain_SelfSpawnConfig pins that a chain agent spawns as an
// ordinary external-ACP agent: command is this binary, args is its ACP
// subcommand, and the only added env var names the declared chain file.
func TestManager_Chain_SelfSpawnConfig(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	chainPath := filepath.Join(t.TempDir(), "agent-fixture.json")
	registerChain(t, ctx, svc, "chain-unit", chainPath)

	mgr := New(svc, WithSelfExecutable(stub))
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "chain-unit", t.TempDir())
	require.NoError(t, err)

	inst := instanceOf(t, mgr, id)
	spawner, ok := inst.spawner.(*agenthost.ExternalACPAgent)
	require.True(t, ok, "a chain agent spawns through the SAME agenthost primitive an external one does")
	require.Equal(t, runtimetypes.ExternalACPTransportStdio, spawner.Config.Transport)
	require.Equal(t, stub, spawner.Config.Command, "the command is this runtime's own binary")
	require.Equal(t, []string{"acp"}, spawner.Config.Args)
	require.Equal(t, map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath}, spawner.Config.Env,
		"ONLY the chain path is set: the child inherits the environment so it shares the one global state")
}

// TestManager_Chain_RunsThroughTheSameBringUp pins that a chain unit reaches
// StateRunning, opens a session, and tears down through the same bringUp
// path as an external agent.
func TestManager_Chain_RunsThroughTheSameBringUp(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerChain(t, ctx, svc, "chain-unit", filepath.Join(t.TempDir(), "agent-fixture.json"))

	mgr := New(svc, WithSelfExecutable(stub))
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "chain-unit", t.TempDir())
	require.NoError(t, err)

	st, err := mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, st.State)
	require.Equal(t, runtimetypes.AgentKindChain, st.Kind)

	// A live connection, not a process-less placeholder: sessions work.
	handle := currentHandle(instanceOf(t, mgr, id))
	require.NotNil(t, handle)
	sessionID, err := mgr.OpenSession(ctx, id, SessionSpec{Cwd: t.TempDir()})
	require.NoError(t, err)
	require.NotEmpty(t, sessionID)

	require.NoError(t, mgr.Stop(id))
	requireConnClosed(t, handle)
	_, err = mgr.Get(id)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestManager_Chain_RejectsUnusableConfig pins that a chain agent with no
// usable chain path fails at spawn rather than starting unable to answer.
func TestManager_Chain_RejectsUnusableConfig(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	mgr := New(svc, WithSelfExecutable("/nonexistent/contenox"))
	t.Cleanup(func() { _ = mgr.Close() })

	_, err := mgr.StartResolved(ctx, &runtimetypes.Agent{Name: "pathless", Kind: runtimetypes.AgentKindChain}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "path is required")
}

// TestManager_Chain_DefaultsToTheRunningExecutable pins that with no
// WithSelfExecutable override, a chain spawn targets os.Executable().
func TestManager_Chain_DefaultsToTheRunningExecutable(t *testing.T) {
	_, _, svc := setupRegistry(t)
	chainPath := filepath.Join(t.TempDir(), "agent-fixture.json")
	agent := &runtimetypes.Agent{Name: "chain-unit", Enabled: true}
	require.NoError(t, agent.SetChainConfig(runtimetypes.ChainConfig{Path: chainPath}))

	self, err := os.Executable()
	require.NoError(t, err)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	spawner, err := mgr.(*manager).chainSpawner(agent, "/tmp/chain-cwd")
	require.NoError(t, err)
	require.Equal(t, self, spawner.(*agenthost.ExternalACPAgent).Config.Command)
	require.Equal(t, "/tmp/chain-cwd", spawner.(*agenthost.ExternalACPAgent).Config.Cwd,
		"a chain agent's sandbox workspace is the caller's cwd (it declares none of its own)")
}

func TestManager_Start_UnknownAgent(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	_, err := mgr.Start(ctx, "does-not-exist", "")
	require.Error(t, err)
}

// countingRegistry counts GetByName calls so a test can pin how many registry
// reads a spawn costs.
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

// TestManager_StartResolved_PerformsNoRegistryRead pins that StartResolved
// performs no registry read of its own (Start resolves once and delegates),
// closing the spawn-path TOCTOU between a caller's Enabled check and the spawn.
func TestManager_StartResolved_PerformsNoRegistryRead(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	agent := registerExternal(t, ctx, svc, "ext-agent", stub)

	counting := &countingRegistry{Service: svc}
	mgr := New(counting)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.StartResolved(ctx, agent, t.TempDir())
	require.NoError(t, err)
	require.Zero(t, counting.reads(), "the kernel spawns what it is handed; it must not re-resolve it")

	st, err := mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, st.State)
	require.Equal(t, agent.ID, st.AgentID)
	require.Equal(t, agent.Name, st.AgentName)

	// Start is the by-name convenience over the same spawn: exactly one read.
	_, err = mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, 1, counting.reads(), "Start resolves once and delegates; it is not a second spawn implementation")
}

// TestManager_StartResolved_RejectsUnusableRecords pins the two ways a
// handed-over record is unusable: nil, or an unsupported kind.
func TestManager_StartResolved_RejectsUnusableRecords(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	_, err := mgr.StartResolved(ctx, nil, "")
	require.Error(t, err)

	_, err = mgr.StartResolved(ctx, &runtimetypes.Agent{Name: "weird", Kind: "no-such-kind"}, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported kind")
}

func TestManager_Get_UnknownID(t *testing.T) {
	_, _, svc := setupRegistry(t)
	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	_, err := mgr.Get("nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestManager_Stop_Idempotent(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	require.NoError(t, mgr.Stop(id))
	require.NoError(t, mgr.Stop(id), "second Stop must be a no-op")
	require.NoError(t, mgr.Stop("never-existed"), "Stop of unknown id must be a no-op")
}

func TestManager_Close_StopsAll(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-a", stub)
	registerExternal(t, ctx, svc, "ext-b", stub)

	mgr := New(svc)

	idA, err := mgr.Start(ctx, "ext-a", t.TempDir())
	require.NoError(t, err)
	idB, err := mgr.Start(ctx, "ext-b", t.TempDir())
	require.NoError(t, err)

	handleA := currentHandle(instanceOf(t, mgr, idA))
	handleB := currentHandle(instanceOf(t, mgr, idB))

	require.NoError(t, mgr.Close())
	requireConnClosed(t, handleA)
	requireConnClosed(t, handleB)

	_, err = mgr.Start(ctx, "ext-a", t.TempDir())
	require.Error(t, err, "Start after Close must be refused")
	require.NoError(t, mgr.Close(), "Close is idempotent")
}

// TestManager_Attach_FanoutAndControllerPermission pins that both viewers on
// a session receive live updates, but only the controller answers a
// permission request.
func TestManager_Attach_FanoutAndControllerPermission(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	sid := openSession(t, mgr, id)

	viewerA := newMockViewer("A")
	viewerB := newMockViewer("B")

	grantedA, err := mgr.Attach(ctx, id, sid, viewerA)
	require.NoError(t, err)
	require.True(t, grantedA, "first viewer of a session becomes controller")

	grantedB, err := mgr.Attach(ctx, id, sid, viewerB)
	require.NoError(t, err)
	require.False(t, grantedB, "second viewer is an observer, not controller")

	// Duplicate viewer id on the same session is rejected.
	_, err = mgr.Attach(ctx, id, sid, newMockViewer("A"))
	require.Error(t, err)

	// The callbacks scenario streams an update then requests a permission.
	reason := promptText(t, mgr, id, sid, "callbacks")
	// The controller cancelled the permission → the stub ends the turn as refusal.
	require.Equal(t, libacp.StopReasonRefusal, reason)

	// Both viewers saw the live "requesting permission..." update (fan-out).
	require.GreaterOrEqual(t, viewerA.updateCount(), 1)
	require.GreaterOrEqual(t, viewerB.updateCount(), 1)
	require.Equal(t, viewerA.updateCount(), viewerB.updateCount(), "both viewers see the same stream")

	// Only the controller answered the permission request.
	require.Equal(t, 1, viewerA.permCount(), "controller answers the permission")
	require.Equal(t, 0, viewerB.permCount(), "an observer is never asked for permission")
}

// TestManager_Attach_JournalReplayThenLive pins that a viewer attached after
// updates flowed receives the backlog by replay, then joins the live stream.
func TestManager_Attach_JournalReplayThenLive(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// Viewer A attaches, then a streaming turn flows several updates to it.
	viewerA := newMockViewer("A")
	_, err = mgr.Attach(ctx, id, sid, viewerA)
	require.NoError(t, err)

	promptText(t, mgr, id, sid, "session_updates")
	backlog := viewerA.updateCount()
	require.Greater(t, backlog, 1, "streaming scenario should emit several updates")

	// Viewer B attaches after the fact and must receive the whole backlog by replay.
	viewerB := newMockViewer("B")
	_, err = mgr.Attach(ctx, id, sid, viewerB)
	require.NoError(t, err)
	require.Equal(t, backlog, viewerB.updateCount(), "late viewer replays the full journal")

	// A subsequent live turn reaches both viewers (B is now in the live fan-out).
	promptText(t, mgr, id, sid, "plain-ack")
	require.Greater(t, viewerB.updateCount(), backlog, "late viewer then joins the live stream")
	require.Equal(t, viewerA.updateCount(), viewerB.updateCount(), "both converge on the same stream")
}

// TestManager_Detach_PromotesNextController pins that detaching the
// controller promotes the next viewer, which then answers permission.
func TestManager_Detach_PromotesNextController(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	viewerA := newMockViewer("A")
	viewerB := newMockViewer("B")
	_, err = mgr.Attach(ctx, id, sid, viewerA)
	require.NoError(t, err)
	_, err = mgr.Attach(ctx, id, sid, viewerB)
	require.NoError(t, err)

	// Detach the controller A → B is promoted.
	require.NoError(t, mgr.Detach(id, sid, "A"))

	reason := promptText(t, mgr, id, sid, "callbacks")
	require.Equal(t, libacp.StopReasonRefusal, reason)
	require.Equal(t, 0, viewerA.permCount(), "detached viewer is never asked")
	require.Equal(t, 1, viewerB.permCount(), "promoted viewer answers permission")
}

// TestManager_NoController_PermissionDenyFallback pins that a permission
// request with no controller hits the built-in deny fallback.
func TestManager_NoController_PermissionDenyFallback(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// Attach then detach the only viewer → the session has no controller.
	viewerA := newMockViewer("A")
	_, err = mgr.Attach(ctx, id, sid, viewerA)
	require.NoError(t, err)
	require.NoError(t, mgr.Detach(id, sid, "A"))

	// A permission request with no controller is denied (cancelled) — the turn
	// ends gracefully as a refusal rather than faulting.
	reason := promptText(t, mgr, id, sid, "callbacks")
	require.Equal(t, libacp.StopReasonRefusal, reason)
	require.Equal(t, 0, viewerA.permCount(), "a detached viewer answers nothing")

	// Detach of an unknown viewer/session is an error the caller may ignore.
	require.Error(t, mgr.Detach(id, sid, "ghost"))
	_, err = mgr.Attach(ctx, "no-such-instance", sid, newMockViewer("Z"))
	require.ErrorIs(t, err, ErrNotFound)
}

// TestUnit_EventSink_UnsupervisedDenyEmitsEvent pins that an unattended
// permission refusal surfaces as a passive EventUnsupervisedDeny, without
// changing the deny outcome itself.
func TestUnit_EventSink_UnsupervisedDenyEmitsEvent(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	var mu sync.Mutex
	var events []Event
	mgr := New(svc, WithEventSink(func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// Attach then detach the only viewer → the session has no controller, so the
	// downstream's permission request during the "callbacks" turn is auto-denied.
	viewerA := newMockViewer("A")
	_, err = mgr.Attach(ctx, id, sid, viewerA)
	require.NoError(t, err)
	require.NoError(t, mgr.Detach(id, sid, "A"))

	reason := promptText(t, mgr, id, sid, "callbacks")
	require.Equal(t, libacp.StopReasonRefusal, reason)

	mu.Lock()
	defer mu.Unlock()
	var deny *Event
	for i := range events {
		if events[i].Kind == EventUnsupervisedDeny {
			deny = &events[i]
			break
		}
	}
	require.NotNil(t, deny, "an unsupervised permission request must emit EventUnsupervisedDeny")
	require.Equal(t, id, deny.InstanceID)
	require.Equal(t, "ext-agent", deny.AgentName)
	require.Equal(t, sid, deny.SessionID)
	require.False(t, deny.Time.IsZero(), "the event carries a timestamp")
}

// TestManager_WatchDog_RestartUpToLimitThenWarning pins that an out-of-band
// death restarts up to the configured limit, then parks in StateWarning.
func TestManager_WatchDog_RestartUpToLimitThenWarning(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc, WithRestart(1)) // one restart allowed, then Warning
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	inst := instanceOf(t, mgr, id)

	h0 := currentHandle(inst)
	require.NotNil(t, h0)

	// Crash #1: out-of-band close, not via Stop, so manualStop stays false.
	require.NoError(t, h0.Close())

	// watchDog restarts: state returns to Running on a different connection.
	require.Eventually(t, func() bool {
		st, _ := mgr.Get(id)
		h := currentHandle(inst)
		return st.State == StateRunning && h != nil && h != h0
	}, 10*time.Second, 25*time.Millisecond, "instance must restart after an unexpected death")

	// Crash #2: the restart budget (1) is now exhausted.
	h1 := currentHandle(inst)
	require.NoError(t, h1.Close())
	require.Eventually(t, func() bool {
		st, _ := mgr.Get(id)
		return st.State == StateWarning
	}, 10*time.Second, 25*time.Millisecond, "a second death past the limit must park in Warning")
}

// TestManager_WatchDog_ManualStopNeverRestarts pins that a manual Stop never
// restarts the instance, even with restart enabled.
func TestManager_WatchDog_ManualStopNeverRestarts(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc, WithRestart(5))
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	inst := instanceOf(t, mgr, id)

	require.NoError(t, mgr.Stop(id))

	// It is removed and never comes back Running.
	_, err = mgr.Get(id)
	require.ErrorIs(t, err, ErrNotFound)
	require.Never(t, func() bool {
		st := inst.status()
		return st.State == StateRunning || st.State == StateStarting
	}, 750*time.Millisecond, 50*time.Millisecond, "a manually stopped instance must never restart")
}

// TestManager_External_UnexpectedExitBecomesError pins that with restart
// disabled (default), an unexpected death is terminal StateError.
func TestManager_External_UnexpectedExitBecomesError(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	// `sh -c "exit 0"` spawns cleanly (Start succeeds) but exits at once,
	// closing the connection — an unexpected death from the Manager's view.
	registerExternal(t, ctx, svc, "dies-immediately", "sh", "-c", "exit 0")

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "dies-immediately", t.TempDir())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		st, gerr := mgr.Get(id)
		return gerr == nil && st.State == StateError
	}, 5*time.Second, 25*time.Millisecond, "an unexpected death with restart disabled becomes Error")

	require.NoError(t, mgr.Stop(id))
	_, err = mgr.Get(id)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestManager_List_JoinsConfigAndRuntime pins that List joins declared
// config with live runtime: idle agents show not-running, started ones show
// their running instance.
func TestManager_List_JoinsConfigAndRuntime(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "idle-agent", stub)
	registerExternal(t, ctx, svc, "live-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	// Nothing started yet: both declared agents appear, both "not running".
	entries, err := mgr.List(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.False(t, e.Running(), "declared-but-idle agent %q must show not running", e.AgentName)
		require.Empty(t, e.Instances)
	}

	// Start one; it now shows a running instance, the other still idle.
	id, err := mgr.Start(ctx, "live-agent", t.TempDir())
	require.NoError(t, err)

	entries, err = mgr.List(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 2, "the declared set is unchanged; only annotation differs")

	byName := map[string]FleetEntry{}
	for _, e := range entries {
		byName[e.AgentName] = e
	}
	require.False(t, byName["idle-agent"].Running())
	require.True(t, byName["live-agent"].Running())
	require.Len(t, byName["live-agent"].Instances, 1)
	require.Equal(t, id, byName["live-agent"].Instances[0].ID)
	require.Equal(t, StateRunning, byName["live-agent"].Instances[0].State)
}

// TestManager_EventSink_FiresOnLifecycle pins that the event sink fires on
// start / attach / detach / stop.
func TestManager_EventSink_FiresOnLifecycle(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	var mu sync.Mutex
	var events []Event
	sink := func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	kinds := func() []EventKind {
		mu.Lock()
		defer mu.Unlock()
		out := make([]EventKind, 0, len(events))
		for _, e := range events {
			out = append(out, e.Kind)
		}
		return out
	}
	hasStateEvent := func(state string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if e.Kind == EventStateChange && e.State == state {
				return true
			}
		}
		return false
	}

	mgr := New(svc, WithEventSink(sink))
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	require.True(t, hasStateEvent(StateRunning), "start fires a Running state_change")

	sid := openSession(t, mgr, id)

	granted, err := mgr.Attach(ctx, id, sid, newMockViewer("A"))
	require.NoError(t, err)
	require.True(t, granted)
	require.Contains(t, kinds(), EventAttach, "attach fires an attach event")

	require.NoError(t, mgr.Detach(id, sid, "A"))
	require.Contains(t, kinds(), EventDetach, "detach fires a detach event")

	require.NoError(t, mgr.Stop(id))
	require.True(t, hasStateEvent(StateStopped), "stop fires a Stopped state_change")
}

// TestManager_OpenSession_PromptRoundTrip pins the OpenSession → Prompt
// round trip through the Manager API, with a viewer observing via Attach,
// and that a connection-less or unknown instance cannot open a session.
func TestManager_OpenSession_PromptRoundTrip(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	// Unknown instance: ErrNotFound.
	_, err := mgr.OpenSession(ctx, "no-such-instance", SessionSpec{Cwd: t.TempDir()})
	require.ErrorIs(t, err, ErrNotFound)

	// A spawner-less instance is live but has no connection to drive. No declared
	// kind produces one any more (chain agents spawn this binary's ACP server),
	// so it is built white-box here to keep the guard covered.
	connLess, err := mgr.(*manager).bringUp(&runtimetypes.Agent{Name: "no-conn", Kind: runtimetypes.AgentKindChain}, nil)
	require.NoError(t, err)
	_, err = mgr.OpenSession(ctx, connLess, SessionSpec{Cwd: t.TempDir()})
	require.ErrorIs(t, err, errNoConn, "an instance with no downstream connection cannot open a session")

	// External instance: OpenSession drives the downstream handshake; Prompt drives a
	// turn whose stream a viewer observes.
	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	sid := openSession(t, mgr, id)

	viewer := newMockViewer("driver-observer")
	granted, err := mgr.Attach(ctx, id, sid, viewer)
	require.NoError(t, err)
	require.True(t, granted)

	// A plain prompt acks with a single agent_message_chunk and ends the turn.
	reason := promptText(t, mgr, id, sid, "plain")
	require.Equal(t, libacp.StopReasonEndTurn, reason)
	require.GreaterOrEqual(t, viewer.updateCount(), 1, "the viewer observes the turn's stream")
}

// TestManager_ConfigOptions_RoundTrip pins that the downstream agent's own
// config-option pickers are captured from session/new and SetConfigOption
// round-trips a confirmed value.
func TestManager_ConfigOptions_RoundTrip(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1"})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// The session/new seed is captured synchronously.
	opts, err := mgr.SessionConfigOptions(id, sid)
	require.NoError(t, err)
	require.Len(t, opts, 1)
	require.Equal(t, "stub-verbosity", opts[0].ID)
	require.Equal(t, "low", opts[0].CurrentValue)

	// A SetConfigOption forwards downstream and adopts the confirmed value.
	require.NoError(t, mgr.SetConfigOption(ctx, id, sid, "stub-verbosity", libacp.StringConfigValue("high")))
	opts, err = mgr.SessionConfigOptions(id, sid)
	require.NoError(t, err)
	require.Len(t, opts, 1)
	require.Equal(t, "high", opts[0].CurrentValue, "the confirmed downstream value is adopted into kernel state")

	// Unknown session: nil, no error (the instance is known).
	none, err := mgr.SessionConfigOptions(id, "no-such-session")
	require.NoError(t, err)
	require.Nil(t, none)
}

// TestManager_SyntheticModeModelOptions_RoundTrip pins that the downstream's
// session Modes and model picker surface as reserved-id synthetic selects,
// and a set on either id translates to session/set_mode / session/set_model.
func TestManager_SyntheticModeModelOptions_RoundTrip(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{
		"ACP_STUB_ADVERTISE_MODES":  "1",
		"ACP_STUB_ADVERTISE_MODELS": "1",
	})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// Synthetic mode select first, synthetic model select second (no downstream own opts).
	opts, err := mgr.SessionConfigOptions(id, sid)
	require.NoError(t, err)
	require.Len(t, opts, 2)
	require.Equal(t, AgentModeConfigOptionID, opts[0].ID)
	require.Equal(t, "code", opts[0].CurrentValue)
	require.Equal(t, AgentModelConfigOptionID, opts[1].ID)
	require.Equal(t, "stub-model-fast", opts[1].CurrentValue)

	// A set on the synthetic mode id → session/set_mode; the confirmed mode is adopted.
	require.NoError(t, mgr.SetConfigOption(ctx, id, sid, AgentModeConfigOptionID, libacp.StringConfigValue("ask")))
	// A set on the synthetic model id → session/set_model; the confirmed model is adopted.
	require.NoError(t, mgr.SetConfigOption(ctx, id, sid, AgentModelConfigOptionID, libacp.StringConfigValue("stub-model-smart")))

	opts, err = mgr.SessionConfigOptions(id, sid)
	require.NoError(t, err)
	require.Len(t, opts, 2)
	require.Equal(t, "ask", opts[0].CurrentValue, "set_mode round-trips onto the synthetic mode option")
	require.Equal(t, "stub-model-smart", opts[1].CurrentValue, "set_model round-trips onto the synthetic model option")
}

// TestManager_AvailableCommands_Captured pins that the downstream
// slash-command menu is captured into per-session state and exposed.
func TestManager_AvailableCommands_Captured(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{"ACP_STUB_ADVERTISE_COMMANDS": "1"})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// The menu arrives as a deferred available_commands_update after session/new, captured
	// on the read loop; wait for it, then assert the exposed menu.
	require.Eventually(t, func() bool {
		cmds, err := mgr.AvailableCommands(id, sid)
		return err == nil && len(cmds) == 2
	}, 10*time.Second, 25*time.Millisecond, "the downstream slash-command menu is captured")

	cmds, err := mgr.AvailableCommands(id, sid)
	require.NoError(t, err)
	require.Equal(t, "review", cmds[0].Name)
	require.Equal(t, "explain", cmds[1].Name)
}

// TestManager_Terminal_RoutesToControllerTerminalServer pins that a
// downstream terminal/* is routed to the controller when it implements
// TerminalServer.
func TestManager_Terminal_RoutesToControllerTerminalServer(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	// Terminal advertised (spec) and the controller serves terminals.
	ctxOpen, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, err := mgr.OpenSession(ctxOpen, id, SessionSpec{Cwd: t.TempDir(), Terminal: true})
	require.NoError(t, err)

	term := newMockTerminalViewer("controller")
	granted, err := mgr.Attach(ctx, id, sid, term)
	require.NoError(t, err)
	require.True(t, granted)

	// The terminal scenario runs a full create/wait/output/release round trip against the
	// controller; it reports the outcome as an agent_message_chunk the controller observes.
	reason := promptText(t, mgr, id, sid, "run terminal")
	require.Equal(t, libacp.StopReasonEndTurn, reason)
	require.Equal(t, 1, term.createCount(), "the controller's TerminalServer serviced terminal/create")
	report := term.lastMessage()
	require.Contains(t, report, "termcap=true")
	require.Contains(t, report, "MOCK-TERMINAL-OUTPUT", "the controller's terminal output flowed back to the agent")
}

// TestManager_Terminal_MethodNotFoundWithoutTerminalServer pins that
// terminal/* is refused with MethodNotFound when the controller does not
// implement TerminalServer.
func TestManager_Terminal_MethodNotFoundWithoutTerminalServer(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	// Terminal advertised, but the controller is a plain viewer (no TerminalServer).
	ctxOpen, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, err := mgr.OpenSession(ctxOpen, id, SessionSpec{Cwd: t.TempDir(), Terminal: true})
	require.NoError(t, err)

	viewer := newMockViewer("plain-controller")
	_, err = mgr.Attach(ctx, id, sid, viewer)
	require.NoError(t, err)

	// terminal/create is refused with MethodNotFound; the stub reports it as a create-error.
	reason := promptText(t, mgr, id, sid, "run terminal")
	require.Equal(t, libacp.StopReasonEndTurn, reason)
	require.True(t, viewerReported(viewer, "create-error"), "a controller without TerminalServer gets MethodNotFound")
}

// TestManager_Terminal_CapabilityWithheld pins that terminals are never
// advertised, and terminal/* is never called, when SessionSpec withholds the
// terminal capability.
func TestManager_Terminal_CapabilityWithheld(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	// SessionSpec withholds the terminal capability (default): the downstream
	// is never told terminals exist, so the scenario reports termcap=false
	// and skips the round trip, even though the controller would serve one.
	sid := openSession(t, mgr, id) // SessionSpec{Terminal: false}
	term := newMockTerminalViewer("controller")
	_, err = mgr.Attach(ctx, id, sid, term)
	require.NoError(t, err)

	reason := promptText(t, mgr, id, sid, "run terminal")
	require.Equal(t, libacp.StopReasonEndTurn, reason)
	require.Equal(t, 0, term.createCount(), "no terminal capability advertised → no terminal/create")
	require.Contains(t, term.lastMessage(), "termcap=false")
}

// TestManager_Cancel_UnblocksInFlightTurn pins that Cancel unblocks an
// in-flight turn.
func TestManager_Cancel_UnblocksInFlightTurn(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	// A controller that blocks on the permission request until its ctx is cancelled.
	ctrl := newBlockingViewer("blocker")
	_, err = mgr.Attach(ctx, id, sid, ctrl)
	require.NoError(t, err)

	type result struct {
		reason libacp.StopReason
		err    error
	}
	done := make(chan result, 1)
	go func() {
		reason, perr := mgr.Prompt(context.Background(), id, sid, []libacp.ContentBlock{libacp.NewTextContent("callbacks")})
		done <- result{reason, perr}
	}()

	// Wait until the downstream's permission request has reached the controller, then cancel.
	select {
	case <-ctrl.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("permission request never reached the controller")
	}
	require.NoError(t, mgr.Cancel(id, sid))

	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.True(t, r.reason == libacp.StopReasonCancelled || r.reason == libacp.StopReasonRefusal,
			"cancel resolves the blocked turn (got %q)", r.reason)
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel did not unblock the in-flight turn")
	}
}

// TestManager_CloseSession_DropsStateNotInstance pins that CloseSession
// drops the session's viewers and captured state without stopping the
// instance.
func TestManager_CloseSession_DropsStateNotInstance(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternalEnv(t, ctx, svc, "ext-agent", stub, map[string]string{"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1"})

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)
	sid := openSession(t, mgr, id)

	_, err = mgr.Attach(ctx, id, sid, newMockViewer("A"))
	require.NoError(t, err)
	_, err = mgr.Attach(ctx, id, sid, newMockViewer("B"))
	require.NoError(t, err)

	st, err := mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, 1, st.Sessions)
	require.Equal(t, 2, st.Viewers)

	opts, err := mgr.SessionConfigOptions(id, sid)
	require.NoError(t, err)
	require.Len(t, opts, 1)

	// CloseSession drops the session's viewers + captured state, but leaves the instance up.
	require.NoError(t, mgr.CloseSession(id, sid))

	st, err = mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, st.State, "the instance stays running after a session closes")
	require.Equal(t, 0, st.Sessions)
	require.Equal(t, 0, st.Viewers)

	opts, err = mgr.SessionConfigOptions(id, sid)
	require.NoError(t, err)
	require.Nil(t, opts, "the closed session's captured state is dropped")

	// The instance can open a fresh session afterwards.
	sid2 := openSession(t, mgr, id)
	require.NotEqual(t, sid, sid2)
}

// TestManager_Status_SessionIDsReflectOpenSessions pins that
// InstanceStatus.SessionIDs reflects exactly the open sessions (sourced from
// the driver, not the viewer hub), sorted, independent of attachment, and
// drops an id only when the session closes.
func TestManager_Status_SessionIDsReflectOpenSessions(t *testing.T) {
	ctx, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, ctx, svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	id, err := mgr.Start(ctx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	// No sessions opened yet: an empty, non-nil slice.
	st, err := mgr.Get(id)
	require.NoError(t, err)
	require.NotNil(t, st.SessionIDs)
	require.Empty(t, st.SessionIDs)

	// An opened session is reported immediately: no prompt has run and no
	// viewer has attached, yet it must still be listed.
	sidA := openSession(t, mgr, id)
	st, err = mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, []string{string(sidA)}, st.SessionIDs,
		"an open-but-silent, unwatched session must still be listed")
	require.Equal(t, 1, st.Sessions)
	require.Equal(t, 0, st.Viewers, "nobody is watching it yet")

	// Attaching a viewer changes the viewer count and nothing about the session set.
	_, err = mgr.Attach(ctx, id, sidA, newMockViewer("A"))
	require.NoError(t, err)

	st, err = mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, []string{string(sidA)}, st.SessionIDs)
	require.Equal(t, 1, st.Sessions)
	require.Equal(t, 1, st.Viewers, "attach is still what makes a viewer")

	sidB := openSession(t, mgr, id)
	_, err = mgr.Attach(ctx, id, sidB, newMockViewer("B"))
	require.NoError(t, err)

	st, err = mgr.Get(id)
	require.NoError(t, err)
	require.Len(t, st.SessionIDs, 2, "both open sessions are reported")
	require.Contains(t, st.SessionIDs, string(sidA))
	require.Contains(t, st.SessionIDs, string(sidB))
	require.True(t, sort.StringsAreSorted(st.SessionIDs), "session ids are sorted for a deterministic snapshot")
	require.Equal(t, 2, st.Sessions)
	require.Equal(t, 2, st.Viewers)

	// Detaching the only viewer of a session leaves the session open — still
	// there to be cancelled or re-adopted; only the viewer count drops.
	require.NoError(t, mgr.Detach(id, sidA, "A"))
	st, err = mgr.Get(id)
	require.NoError(t, err)
	require.Len(t, st.SessionIDs, 2, "detaching a viewer does not close a session")
	require.Contains(t, st.SessionIDs, string(sidA))
	require.Equal(t, 1, st.Viewers, "only the viewer went away")

	// Closing it is what removes it.
	require.NoError(t, mgr.CloseSession(id, sidA))
	st, err = mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, []string{string(sidB)}, st.SessionIDs, "a closed session is dropped")
	require.Equal(t, 1, st.Sessions)
}

// TestManager_OwnershipSurvivesCallerCtxCancel pins that the instance is
// bound to the Manager's root context, not the ctx passed to Start.
func TestManager_OwnershipSurvivesCallerCtxCancel(t *testing.T) {
	_, _, svc := setupRegistry(t)
	stub := buildStubAgent(t)
	registerExternal(t, context.Background(), svc, "ext-agent", stub)

	mgr := New(svc)
	t.Cleanup(func() { _ = mgr.Close() })

	callerCtx, cancel := context.WithCancel(context.Background())
	id, err := mgr.Start(callerCtx, "ext-agent", t.TempDir())
	require.NoError(t, err)

	inst := instanceOf(t, mgr, id)
	handle := currentHandle(inst)
	cancel()

	// Negative proof: cancelling the caller ctx must not tear the instance down.
	require.Never(t, func() bool {
		st, gerr := mgr.Get(id)
		return gerr != nil || st.State != StateRunning
	}, 750*time.Millisecond, 50*time.Millisecond, "instance must survive caller-ctx cancellation")

	// Positive proof: the subprocess is genuinely alive — it answers a fresh ACP
	// initialize over the connection the Manager still owns. Use a fresh ctx.
	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer initCancel()
	resp, err := handle.Conn.Initialize(initCtx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "agentinstance-test", Version: "test"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.AgentInfo)
	require.Equal(t, "acp-stub-agent", resp.AgentInfo.Name)
}
