package acpsvc

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This file exercises the Manager-backed external-agent path: an external session
// ATTACHES to an agentinstance.Manager-owned instance whose subprocess lives OFF any
// single connection. It proves the point of the whole rewire — the agent's process
// (and thus its context) survives a client disconnect/reload, a reloaded session
// re-attaches to the SAME still-running instance, only a delete stops it — and that
// the nil-Instances fallback is byte-for-byte today's connCtx-owned spawn. The
// downstream side is the hermetic in-repo acp-stub-agent; there is no LLM backend.

// instancesFixture wires a Manager-backed acpsvc Deps and opens MULTIPLE loopback ACP
// connections against it, mirroring serve (one acpsvc.New factory fronting many WS
// connections behind ONE shared agentinstance.Manager + DB). It is the harness for the
// re-attach story: session/new on connection A, A dropped, session/load on connection
// B re-attaching to the same live instance.
type instancesFixture struct {
	t       *testing.T
	db      libdb.DBManager
	mgr     agentinstance.Manager
	factory libacp.AgentFactory
}

func newInstancesFixture(t *testing.T) *instancesFixture {
	t.Helper()
	return newInstancesFixtureWith(t, func(db libdb.DBManager) agentinstance.Manager {
		return agentinstance.New(agentregistryservice.New(db))
	})
}

// newInstancesFixtureWith is newInstancesFixture with the Manager supplied by the
// caller — so a test can wire kernel Options (an event sink, a journal size) or swap in
// a double whose Get/Attach it fully controls. build receives the fixture's DB because
// the real Manager resolves declared agents against it.
func newInstancesFixtureWith(t *testing.T, build func(libdb.DBManager) agentinstance.Manager) *instancesFixture {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "instances.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	bus := libbus.NewInMem()
	mgr := build(db)
	factory := New(Deps{
		Engine:        &enginesvc.Engine{Bus: bus},
		DB:            db,
		ChainRegistry: &ChainRegistry{defaultChain: &taskengine.TaskChainDefinition{}},
		WorkspaceID:   "loopback-ws",
		SessionRouter: NewSessionRouter(),
		Instances:     mgr,
	})
	t.Cleanup(func() {
		// LIFO after every connection has dropped: stop all surviving instances
		// (killing their subprocesses) then close the DB — the leak-free teardown.
		_ = mgr.Close()
		_ = db.Close()
	})
	return &instancesFixture{t: t, db: db, mgr: mgr, factory: factory}
}

// instConn is one live loopback ACP connection (upstream client <-> real Transport) on
// the fixture's shared factory.
type instConn struct {
	t          *testing.T
	tr         *Transport
	client     *libacp.ClientSideConnection
	lc         *loopbackClient
	cancel     context.CancelFunc
	agentDone  chan error
	clientDone chan error
	dropped    bool
}

func (f *instancesFixture) connect() *instConn {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	var tr *Transport
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		a := f.factory(c)
		tr = a.(*Transport)
		return a
	})
	lc := newLoopbackClient()
	clientConn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client {
		return lc
	})

	agentDone := make(chan error, 1)
	clientDone := make(chan error, 1)
	go func() { agentDone <- agentConn.Run(ctx) }()
	go func() { clientDone <- clientConn.Run(ctx) }()

	ic := &instConn{t: f.t, tr: tr, client: clientConn, lc: lc, cancel: cancel, agentDone: agentDone, clientDone: clientDone}
	f.t.Cleanup(ic.drop)
	return ic
}

// drop simulates the upstream connection ending: it cancels the connection ctx, which
// fires connCtx (the SOLE WebSocket teardown hook — Transport.Close is never called on
// a WS drop), and waits for both run loops to exit. Idempotent.
func (ic *instConn) drop() {
	if ic.dropped {
		return
	}
	ic.dropped = true
	ic.cancel()
	select {
	case <-ic.agentDone:
	case <-time.After(2 * time.Second):
		ic.t.Error("agent connection did not shut down after drop")
	}
	select {
	case <-ic.clientDone:
	case <-time.After(2 * time.Second):
		ic.t.Error("client connection did not shut down after drop")
	}
}

func (ic *instConn) externalDriver(sid libacp.SessionID) *externalDriver {
	return externalDriverOf(ic.t, ic.tr, sid)
}

// externalDriverOf fetches (white-box) the external driver backing sid on tr, so a
// test can assert on its ownership fields (instanceID, handle) directly.
func externalDriverOf(t *testing.T, tr *Transport, sid libacp.SessionID) *externalDriver {
	t.Helper()
	tr.sessionMu.Lock()
	entry := tr.sessions[sid]
	tr.sessionMu.Unlock()
	require.NotNil(t, entry, "session %q not open on this connection", sid)
	ed, ok := entry.driver.(*externalDriver)
	require.True(t, ok, "session %q must be backed by an external driver", sid)
	return ed
}

func extInstanceID(ed *externalDriver) string {
	ed.mu.Lock()
	defer ed.mu.Unlock()
	return ed.instanceID
}

func extHandle(ed *externalDriver) *agenthost.Handle {
	ed.mu.Lock()
	defer ed.mu.Unlock()
	return ed.handle
}

// liveInstances counts the Manager's currently-running instances across the
// config+runtime join List returns. Under the R1 kernel List reports one FleetEntry
// per DECLARED agent (idle or not), so a running-instance count sums each entry's
// live Instances rather than counting entries.
func liveInstances(t *testing.T, mgr agentinstance.Manager) int {
	t.Helper()
	entries, err := mgr.List(context.Background())
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		n += len(e.Instances)
	}
	return n
}

// TestLoopback_ExternalInstance_SurvivesConnectionDrop is the assertion this rewire
// exists for: a Manager-backed external session's downstream process is owned by the
// Manager (the driver holds NO handle, only an instance id), and a client disconnect
// (connCtx cancel) does NOT tear it down — it stays Running and answers again through a
// fresh connection.
func TestLoopback_ExternalInstance_SurvivesConnectionDrop(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-survive", nil)
	cwd := t.TempDir()
	ctx := context.Background()

	c1 := f.connect()
	_, err := c1.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c1.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	ed := c1.externalDriver(newResp.SessionID)
	instanceID := extInstanceID(ed)
	require.NotEmpty(t, instanceID, "a Manager-backed external session records its instance id")
	require.Nil(t, extHandle(ed), "the driver must NOT own the process on the Manager path (the instance does)")

	st, err := f.mgr.Get(instanceID)
	require.NoError(t, err)
	require.Equal(t, agentinstance.StateRunning, st.State)

	// It answers on the first connection.
	promptResp, err := c1.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	// Drop the connection (connCtx cancel — the WS teardown hook).
	c1.drop()

	// The instance SURVIVES: never leaves Running (had the subprocess been bound to
	// connCtx, the Manager's monitor would have flipped it to error within this window).
	require.Never(t, func() bool {
		st, gerr := f.mgr.Get(instanceID)
		return gerr != nil || st.State != agentinstance.StateRunning
	}, 500*time.Millisecond, 50*time.Millisecond, "the instance must survive the connection drop")
	require.Equal(t, 1, liveInstances(t, f.mgr))

	// A fresh connection re-attaches (session/load) and it answers AGAIN.
	c2 := f.connect()
	_, err = c2.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = c2.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID: newResp.SessionID,
		Cwd:       cwd,
	})
	require.NoError(t, err)
	promptResp2, err := c2.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("again")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp2.StopReason,
		"the surviving instance must answer a prompt on the re-attached connection")
}

// TestLoopback_ExternalInstance_SessionLoadReattachesSameInstance pins that a
// session/load after a disconnect re-attaches to the SAME still-running instance
// (identical instance id, and NO second instance spawned) instead of freshly
// respawning — the downstream agent's context is preserved across the reload.
func TestLoopback_ExternalInstance_SessionLoadReattachesSameInstance(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-reattach", nil)
	cwd := t.TempDir()
	ctx := context.Background()

	c1 := f.connect()
	_, err := c1.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c1.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	instanceID := extInstanceID(c1.externalDriver(newResp.SessionID))
	require.NotEmpty(t, instanceID)

	_, err = c1.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("first")},
	})
	require.NoError(t, err)
	c1.drop()

	c2 := f.connect()
	_, err = c2.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = c2.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID: newResp.SessionID,
		Cwd:       cwd,
	})
	require.NoError(t, err)
	// The re-attach is lazy: the first prompt after a load drives it.
	promptResp, err := c2.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("second")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	ed2 := c2.externalDriver(newResp.SessionID)
	require.Equal(t, instanceID, extInstanceID(ed2),
		"session/load must re-attach to the SAME instance, not spawn a fresh one")
	require.Nil(t, extHandle(ed2), "a re-attached driver does not own the process")
	require.Equal(t, 1, liveInstances(t, f.mgr), "re-attach must NOT spawn a second instance")
}

// TestLoopback_ExternalInstance_DeleteStopsInstance proves the teardown asymmetry: a
// plain disconnect/close only detaches (the instance survives — the other tests), but
// DeleteSession actually STOPS the instance and removes it from the Manager, leak-free.
func TestLoopback_ExternalInstance_DeleteStopsInstance(t *testing.T) {
	f := newInstancesFixture(t)
	agentName := registerStubAgentInDB(t, f.db, "claude-stub-delete", nil)
	cwd := t.TempDir()
	ctx := context.Background()

	c1 := f.connect()
	_, err := c1.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := c1.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	instanceID := extInstanceID(c1.externalDriver(newResp.SessionID))
	require.NotEmpty(t, instanceID)
	require.Equal(t, 1, liveInstances(t, f.mgr))

	_, err = c1.client.DeleteSession(ctx, libacp.DeleteSessionRequest{SessionID: newResp.SessionID})
	require.NoError(t, err)

	require.Equal(t, 0, liveInstances(t, f.mgr), "DeleteSession must stop the Manager instance")
	_, gerr := f.mgr.Get(instanceID)
	require.ErrorIs(t, gerr, agentinstance.ErrNotFound)
	// Truly gone, not merely detached: every Manager entry point that resolves the
	// instance id fails — observing it (Attach) AND driving it (OpenSession). The
	// Manager exposes no raw-connection accessor to probe; these are the surfaces a
	// consumer actually holds.
	_, aerr := f.mgr.Attach(ctx, instanceID, "any-session", newExternalBridge(c1.tr, "any-session", true))
	require.ErrorIs(t, aerr, agentinstance.ErrNotFound)
	_, oerr := f.mgr.OpenSession(ctx, instanceID, agentinstance.SessionSpec{Cwd: cwd})
	require.ErrorIs(t, oerr, agentinstance.ErrNotFound)
}

// TestLoopback_ExternalInstance_NilInstancesFallsBackToConnCtxSpawn pins the fallback
// contract: with Deps.Instances nil the external path is today's connCtx-owned spawn
// (the driver OWNS a live handle, holds NO instance id); with a Manager wired it is the
// inverse (the driver holds an instance id, NO handle — the Manager owns the process).
func TestLoopback_ExternalInstance_NilInstancesFallsBackToConnCtxSpawn(t *testing.T) {
	ctx := context.Background()

	// nil Instances (the stdio-parity default harness): connCtx-owned subprocess.
	h := newLoopbackHarness(t)
	require.Nil(t, h.tr.deps.Instances, "the default harness must wire no Manager")
	nilAgent := registerStubAgent(t, h, "claude-stub-nil")
	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	nilResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(nilAgent),
	})
	require.NoError(t, err)
	nilEd := externalDriverOf(t, h.tr, nilResp.SessionID)
	require.Empty(t, extInstanceID(nilEd), "no Manager => no instance id")
	require.NotNil(t, extHandle(nilEd), "no Manager => the driver owns the connCtx-bound subprocess")

	// Instances wired: the Manager owns the process; the driver holds no handle.
	f := newInstancesFixture(t)
	mgrAgent := registerStubAgentInDB(t, f.db, "claude-stub-mgr", nil)
	c := f.connect()
	_, err = c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	mgrResp, err := c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(mgrAgent),
	})
	require.NoError(t, err)
	mgrEd := c.externalDriver(mgrResp.SessionID)
	require.NotEmpty(t, extInstanceID(mgrEd), "a Manager => the session records its instance id")
	require.Nil(t, extHandle(mgrEd), "a Manager => the driver does not own the process")
}

// TestLoopback_ExternalInstance_DisabledAgentRejected proves the Manager-owned
// spawn path refuses a disabled agent through the same shared judgment
// (agentregistryservice.ResolveForSpawn, called from resolveExternalAgent) the
// connCtx-owned path and fleetservice.Dispatch use — the actual C5 gap
// fleet-consolidation.md's D6 named: before this change
// agentinstance.Manager.Start had no notion of Enabled, so a disabled agent's
// session/new against a Manager-backed acpsvc would have reached Start and
// spawned anyway. Uses /bin/true as the command: resolution is refused before
// anything is ever spawned, so no real stub binary is needed.
func TestLoopback_ExternalInstance_DisabledAgentRejected(t *testing.T) {
	f := newInstancesFixture(t)
	ctx := context.Background()

	const agentName = "claude-stub-disabled-mgr"
	svc := agentregistryservice.New(f.db)
	agent := &runtimetypes.Agent{Name: agentName, Enabled: false}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "/bin/true",
	}))
	require.NoError(t, svc.Create(ctx, agent))

	c := f.connect()
	_, err := c.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = c.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")
	require.Equal(t, 0, liveInstances(t, f.mgr),
		"a refused agent must never reach agentinstance.Manager.Start")
}
