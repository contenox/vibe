package acpsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/relayacp"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/librelay"
	"github.com/stretchr/testify/require"
)

// fleetInstance is the runtime identity the tunnel stamps on every frame.
const fleetInstance = "inst-fleet"

// phoneAttachment is the relay's side of one attachment: the io.ReadWriteCloser
// a real client connection runs on, whose writes become inbound frames for the
// tunnel and whose reads serve the frames the tunnel sent back.
type phoneAttachment struct {
	session string
	handle  func(context.Context, librelay.Frame)

	in   chan json.RawMessage
	done chan struct{}
	once sync.Once

	pending  []byte
	outbound []byte
}

func newPhoneAttachment(session string, handle func(context.Context, librelay.Frame)) *phoneAttachment {
	return &phoneAttachment{
		session: session,
		handle:  handle,
		in:      make(chan json.RawMessage, 256),
		done:    make(chan struct{}),
	}
}

// offer queues one payload the tunnel emitted for this attachment.
func (p *phoneAttachment) offer(payload json.RawMessage) {
	select {
	case <-p.done:
	case p.in <- payload:
	default:
	}
}

func (p *phoneAttachment) Read(b []byte) (int, error) {
	if len(p.pending) == 0 {
		select {
		case <-p.done:
			return 0, io.EOF
		case msg := <-p.in:
			p.pending = append(append(make([]byte, 0, len(msg)+1), msg...), '\n')
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *phoneAttachment) Write(b []byte) (int, error) {
	select {
	case <-p.done:
		return 0, io.ErrClosedPipe
	default:
	}
	p.outbound = append(p.outbound, b...)
	for {
		i := bytes.IndexByte(p.outbound, '\n')
		if i < 0 {
			return len(b), nil
		}
		if line := p.outbound[:i]; len(bytes.TrimSpace(line)) > 0 {
			payload := make(json.RawMessage, len(line))
			copy(payload, line)
			p.handle(context.Background(), librelay.Frame{
				Type:     librelay.TypeACPMessage,
				Instance: fleetInstance,
				Session:  p.session,
				Payload:  payload,
			})
		}
		p.outbound = append(p.outbound[:0], p.outbound[i+1:]...)
	}
}

func (p *phoneAttachment) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

var _ io.ReadWriteCloser = (*phoneAttachment)(nil)

// fleetPeer is one client of the shared engine: the transport serving it, the
// client connection driving it, and the scripted client answering its reverse
// calls.
type fleetPeer struct {
	tr     *Transport
	client *libacp.ClientSideConnection
	lc     *loopbackClient
	// built hands over the transport the factory made for this peer. An
	// attachment's transport does not exist until its client has spoken, so
	// it is collected on first use rather than at construction.
	built <-chan *Transport
}

// ensureTransport resolves the transport serving this peer, waiting for the
// factory to build it the first time.
func (p *fleetPeer) ensureTransport(t *testing.T) *Transport {
	t.Helper()
	if p.tr != nil {
		return p.tr
	}
	select {
	case p.tr = <-p.built:
	case <-time.After(10 * time.Second):
		t.Fatal("no transport was built for this peer")
	}
	return p.tr
}

// permissionCount reports how many session/request_permission calls this client
// has been shown.
func (p *fleetPeer) permissionCount() int {
	p.lc.permMu.Lock()
	defer p.lc.permMu.Unlock()
	return len(p.lc.permReqs)
}

// newSession opens a session and returns both identifiers: the ACP one the
// client addresses and the contenox one the router keys on.
func (p *fleetPeer) newSession(t *testing.T) (libacp.SessionID, string) {
	t.Helper()
	ctx := context.Background()
	_, err := p.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	resp, err := p.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	p.lc.drain(t, 1)
	p.ensureTransport(t)
	return resp.SessionID, p.internalID(t, resp.SessionID)
}

func (p *fleetPeer) internalID(t *testing.T, sid libacp.SessionID) string {
	t.Helper()
	p.tr.sessionMu.Lock()
	defer p.tr.sessionMu.Unlock()
	entry, ok := p.tr.sessions[sid]
	require.True(t, ok, "session %s is not open on this transport", sid)
	require.NotEmpty(t, entry.InternalSessionID)
	return entry.InternalSessionID
}

// promptRaising installs an agent whose turn raises one approval through the
// shared router, then prompts, so the request travels the production path
// (engine -> router -> owning transport) rather than being aimed by the test.
func (p *fleetPeer) promptRaising(t *testing.T, router *SessionRouter, sid libacp.SessionID, toolCallID string) (bool, error) {
	t.Helper()
	var allowed bool
	var askErr error
	p.tr.sessionMu.Lock()
	p.tr.sessions[sid].driver.(*nativeDriver).agent = &loopbackAgent{
		promptFunc: func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			askCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, req.SessionID)
			allowed, askErr = router.AskApproval(askCtx, hitlservice.ApprovalRequest{
				ToolCallID: toolCallID,
				ToolName:   "local_fs.write_file",
				Args:       map[string]any{"path": "/tmp/fleet/x"},
			})
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	}
	p.tr.sessionMu.Unlock()

	_, err := p.client.Prompt(context.Background(), libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the gated thing")},
	})
	require.NoError(t, err)
	return allowed, askErr
}

// fleet is the shared engine, its router, and the two clients on it.
type fleet struct {
	router *SessionRouter
	tunnel *relayacp.Tunnel
	desk   *fleetPeer
	built  <-chan *Transport
	// db is the store every peer's transport shares, so a test can write the
	// durable rows an attach is supposed to react to.
	db libdb.DBManager

	mu    sync.Mutex
	pipes map[string]*phoneAttachment
}

// attach brings up one relay attachment under session and returns the peer
// behind it.
func (f *fleet) attach(t *testing.T, session string) *fleetPeer {
	t.Helper()
	pipe := newPhoneAttachment(session, f.tunnel.Handle)
	f.mu.Lock()
	f.pipes[session] = pipe
	f.mu.Unlock()

	lc := newLoopbackClient()
	conn := libacp.NewClientSideConnection(pipe, func(*libacp.ClientSideConnection) libacp.Client { return lc })
	done := make(chan error, 1)
	go func() { done <- conn.Run(context.Background()) }()
	t.Cleanup(func() {
		_ = pipe.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("attachment client did not shut down")
		}
	})
	return &fleetPeer{client: conn, lc: lc, built: f.built}
}

// newFleet wires one engine, one router and one desk connection, plus a tunnel
// ready to accept attachments.
func newFleet(t *testing.T, withFleetDeps ...func(*Deps, libdb.DBManager)) *fleet {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleet.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{
		EventPoll:   5 * time.Millisecond,
		RequestPoll: 5 * time.Millisecond,
	})

	router := NewSessionRouter()
	built := make(chan *Transport, 8)
	deps := Deps{
		Engine:        &enginesvc.Engine{Bus: bus},
		DB:            db,
		ChainRegistry: &ChainRegistry{defaultChain: &taskengine.TaskChainDefinition{}},
		WorkspaceID:   "fleet-ws",
		SessionRouter: router,
	}
	for _, apply := range withFleetDeps {
		apply(&deps, db)
	}
	base := New(deps)
	factory := func(c *libacp.AgentSideConnection) libacp.Agent {
		a := base(c)
		built <- a.(*Transport)
		return a
	}

	f := &fleet{router: router, db: db, pipes: map[string]*phoneAttachment{}}
	tunnel, err := relayacp.New(relayacp.Config{
		Instance: fleetInstance,
		Factory:  factory,
		Send: func(fr librelay.Frame) error {
			f.mu.Lock()
			pipe := f.pipes[fr.Session]
			f.mu.Unlock()
			if pipe != nil {
				pipe.offer(fr.Payload)
			}
			return nil
		},
	})
	require.NoError(t, err)
	f.tunnel = tunnel

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentConn := libacp.NewAgentSideConnection(&wirePipe{r: agentR, w: agentW}, factory)
	deskLC := newLoopbackClient()
	deskConn := libacp.NewClientSideConnection(&wirePipe{r: clientR, w: clientW}, func(*libacp.ClientSideConnection) libacp.Client { return deskLC })

	agentDone := make(chan error, 1)
	deskDone := make(chan error, 1)
	go func() { agentDone <- agentConn.Run(ctx) }()
	go func() { deskDone <- deskConn.Run(ctx) }()

	f.built = built
	f.desk = &fleetPeer{client: deskConn, lc: deskLC, built: built}
	f.desk.ensureTransport(t)

	t.Cleanup(func() {
		tunnel.Close()
		cancel()
		<-agentDone
		<-deskDone
		require.NoError(t, bus.Close())
		require.NoError(t, db.Close())
	})
	return f
}

// TestFleet_ApprovalGoesToTheClientDrivingTheSession is the defect this whole
// seam closes.
func TestFleet_ApprovalGoesToTheClientDrivingTheSession(t *testing.T) {
	f := newFleet(t)
	phone := f.attach(t, "att-phone")

	phoneSID, _ := phone.newSession(t)
	require.NotSame(t, f.desk.tr, phone.tr, "an attachment must be its own transport")
	phone.tr.sessionMu.Lock()
	_, open := phone.tr.sessions[phoneSID]
	phone.tr.sessionMu.Unlock()
	require.True(t, open, "the session must be open on the attachment's own transport")

	phone.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	allowed, err := phone.promptRaising(t, f.router, phoneSID, "call-remote-1")
	require.NoError(t, err)
	require.True(t, allowed, "the phone answered allow; the verdict must come back from the phone")

	req, ok := phone.lc.lastPermissionRequest()
	require.True(t, ok, "the remote client must have been shown the card")
	require.Equal(t, "call-remote-1", req.ToolCall.ToolCallID)
	require.Equal(t, phoneSID, req.SessionID)
	require.Zero(t, f.desk.permissionCount(), "the desk must never see a card raised by remote work")
}

// TestFleet_StdioIsUnchangedWithNothingAttached pins the other direction: a
// runtime nobody has attached to behaves exactly as it did before the router
// existed.
func TestFleet_StdioIsUnchangedWithNothingAttached(t *testing.T) {
	f := newFleet(t)
	require.Zero(t, f.tunnel.Len(), "no attachment may exist before a frame arrives")

	deskSID, _ := f.desk.newSession(t)
	f.desk.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	allowed, err := f.desk.promptRaising(t, f.router, deskSID, "call-desk-1")
	require.NoError(t, err)
	require.True(t, allowed)
	req, ok := f.desk.lc.lastPermissionRequest()
	require.True(t, ok, "the stdio client must still be shown its own cards")
	require.Equal(t, "call-desk-1", req.ToolCall.ToolCallID)

	_, err = f.router.AskApproval(
		context.WithValue(context.Background(), runtimetypes.SessionIDContextKey, "no-such-session"),
		hitlservice.ApprovalRequest{ToolCallID: "call-orphan", ToolName: "local_fs.write_file"},
	)
	require.ErrorIs(t, err, ErrNoBoundSession, "an unheld session must fall through to the caller's own policy")
}

// TestFleet_TwoClientsOnOneSessionAreBothAskedAndFirstAnswerWins replaces a
// single-owner rule this test previously pinned: a session is shared, so every
// client holding it is shown the card, and the first real answer resolves the
// question for all of them.
func TestFleet_TwoClientsOnOneSessionAreBothAskedAndFirstAnswerWins(t *testing.T) {
	f := newFleet(t)
	phone := f.attach(t, "att-phone")

	phoneSID, internalID := phone.newSession(t)
	require.Same(t, phone.tr, mustOwner(t, f.router, internalID), "the client that opened the session holds it")

	ctx := context.Background()
	_, err := f.desk.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = f.desk.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  phoneSID,
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.Len(t, f.router.transportsFor(internalID), 2, "loading a session joins it rather than taking it")

	releaseDesk := f.desk.lc.holdPermission()
	defer releaseDesk()
	phone.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	deskCardsBefore := f.desk.permissionCount()

	allowed, err := phone.promptRaising(t, f.router, phoneSID, "call-driven-by-phone")
	require.NoError(t, err)
	require.True(t, allowed, "the answer that arrived is the verdict")

	req, ok := phone.lc.lastPermissionRequest()
	require.True(t, ok, "the client that fired the turn is asked")
	require.Equal(t, "call-driven-by-phone", req.ToolCall.ToolCallID)

	// Eventually, not Greater: the losing ask runs on its own goroutine, so the
	// winning verdict can return before the desk's card has been counted. A bare
	// comparison here passes on a fast machine and fails under load, which is
	// what it did.
	require.Eventually(t, func() bool { return f.desk.permissionCount() > deskCardsBefore }, 5*time.Second, 10*time.Millisecond,
		"the other screen is asked the same question")
	require.Eventually(t, func() bool { return f.desk.lc.cancelledPermissions() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the losing card is torn down rather than left waiting on a question already answered")
}

// TestFleet_DetachStopsTheDetachedClientBeingTheApprovalTarget joins the two
// halves of this change.
func TestFleet_DetachStopsTheDetachedClientBeingTheApprovalTarget(t *testing.T) {
	f := newFleet(t)
	phone := f.attach(t, "att-phone")

	_, internalID := phone.newSession(t)
	require.Same(t, phone.tr, mustOwner(t, f.router, internalID))

	f.tunnel.Handle(context.Background(), librelay.Frame{
		Type:     librelay.TypeACPDetach,
		Instance: fleetInstance,
		Session:  "att-phone",
	})
	require.Eventually(t, func() bool { return f.tunnel.Len() == 0 }, 10*time.Second, time.Millisecond,
		"the detached attachment must be reclaimed")

	require.Eventually(t, func() bool {
		_, ok := f.router.transportFor(internalID)
		return !ok
	}, 10*time.Second, time.Millisecond, "a detached client must stop being the approval target")

	_, err := f.router.AskApproval(
		context.WithValue(context.Background(), runtimetypes.SessionIDContextKey, internalID),
		hitlservice.ApprovalRequest{ToolCallID: "call-after-detach", ToolName: "local_fs.write_file"},
	)
	require.ErrorIs(t, err, ErrNoBoundSession)
	require.Zero(t, f.desk.permissionCount(), "the desk inherits no card from a detached client")
}

// mustOwner returns the transport currently registered for a contenox session,
// failing the test when none is.
func mustOwner(t *testing.T, r *SessionRouter, contenoxSessionID string) *Transport {
	t.Helper()
	owner, ok := r.transportFor(contenoxSessionID)
	require.True(t, ok, "no transport holds %s", contenoxSessionID)
	return owner
}

// waitForUpdate reports whether a session/update carrying text arrives on this
// client within the window, draining whatever precedes it (a load replays the
// transcript before anything new is sent).
func (p *fleetPeer) waitForUpdate(t *testing.T, text string, within time.Duration) bool {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case n := <-p.lc.updates:
			if n.Update.Content != nil && n.Update.Content.Text == text {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestFleet_ATurnOnOneClientRendersOnTheOther is the property the surfaces are
// expected to have and did not: a session is one thing several screens watch.
func TestFleet_ATurnOnOneClientRendersOnTheOther(t *testing.T) {
	f := newFleet(t)
	phone := f.attach(t, "att-phone")

	phoneSID, internalID := phone.newSession(t)

	ctx := context.Background()
	_, err := f.desk.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	_, err = f.desk.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  phoneSID,
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.Len(t, f.router.transportsFor(internalID), 2)

	const line = "this is what the phone is showing"
	phone.tr.sendUpdate(ctx, libacp.SessionNotification{
		SessionID: phoneSID,
		Update:    libacp.NewAgentMessageChunk(line),
	})

	require.True(t, phone.waitForUpdate(t, line, 5*time.Second),
		"the connection running the turn still receives its own updates")
	require.True(t, f.desk.waitForUpdate(t, line, 5*time.Second),
		"a client holding the same session sees the turn as it happens")
}

// TestFleet_MirroringStopsWhenAClientLeaves pins the other half: the mirror
// follows the router, so a detached client stops being written to.
func TestFleet_MirroringStopsWhenAClientLeaves(t *testing.T) {
	f := newFleet(t)
	phone := f.attach(t, "att-phone")

	phoneSID, internalID := phone.newSession(t)
	require.Len(t, f.router.transportsFor(internalID), 1)

	phone.tr.releaseSessionRouting()
	require.Empty(t, f.router.transportsFor(internalID), "a departed client holds nothing")

	f.desk.tr = f.desk.ensureTransport(t)
	require.NotPanics(t, func() {
		f.router.mirror(f.desk.tr, internalID, libacp.SessionNotification{
			SessionID: phoneSID,
			Update:    libacp.NewAgentMessageChunk("into the void"),
		})
	}, "mirroring a session nobody holds is a no-op, not a write to a dead transport")
}
