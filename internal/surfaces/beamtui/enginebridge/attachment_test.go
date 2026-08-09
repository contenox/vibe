package enginebridge

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// This file covers the property Bridge.AgentFactory exists for. A surface that
// holds a relay connection serves remote clients from the same factory its own
// loopback is served by, so one engine drives two connections at once — the
// terminal the operator is looking at, and whatever attached. A permission
// request raised by remote work must be answered on the connection that raised
// it and must never surface as a card on the operator's screen, where a second
// human would answer a question already asked of the first.

// acpSessionIdentity is the identity acpsvc stores its sessions under. A test
// reads sessions back through the same service the surface does, because the
// contenox session id the router keys on is not on the ACP wire.
const acpSessionIdentity = "acp-client"

// cardBudget bounds how long a test waits for a card that should arrive, and
// how long it waits to be sure one that should not has not.
const cardBudget = 10 * time.Second

// attachedClient is the remote end of one attachment: a real libacp client
// that records every permission card it is shown and answers with a scripted
// verdict, so "which connection was asked" is observable rather than assumed.
type attachedClient struct {
	libacp.UnimplementedClient

	allow bool

	mu    sync.Mutex
	cards []libacp.RequestPermissionRequest
}

func (c *attachedClient) RequestPermission(_ context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.cards = append(c.cards, req)
	c.mu.Unlock()

	option := approvalflow.OptionDeny
	if c.allow {
		option = approvalflow.OptionAllow
	}
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: option},
	}, nil
}

func (c *attachedClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cards)
}

func (c *attachedClient) last() (libacp.RequestPermissionRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cards) == 0 {
		return libacp.RequestPermissionRequest{}, false
	}
	return c.cards[len(c.cards)-1], true
}

// attachment is one remote client and the transport serving it.
type attachment struct {
	client    *attachedClient
	transport *acpsvc.Transport
	session   libacp.SessionID
}

// attach serves one more ACP connection from the Bridge's own factory, which
// is what the relay tunnel does with every remote client, and opens a session
// on it. The connection is torn down with the test.
func (h *harness) attach(ctx context.Context, allow bool) *attachment {
	h.t.Helper()

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &duplexPipe{r: agentR, w: agentW}
	clientSide := &duplexPipe{r: clientR, w: clientW}

	var transport *acpsvc.Transport
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent := h.bridge.AgentFactory()(c)
		transport, _ = agent.(*acpsvc.Transport)
		return agent
	})
	require.NotNil(h.t, transport, "the bridge's factory must build a transport for an attachment")

	remote := &attachedClient{allow: allow}
	clientConn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client { return remote })

	runCtx, cancel := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	clientDone := make(chan error, 1)
	go func() { agentDone <- agentConn.Run(runCtx) }()
	go func() { clientDone <- clientConn.Run(runCtx) }()
	h.t.Cleanup(func() {
		cancel()
		_ = agentSide.Close()
		_ = clientSide.Close()
		<-agentDone
		<-clientDone
	})

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(h.t, err)
	resp, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: h.dir, McpServers: []libacp.McpServer{}})
	require.NoError(h.t, err)

	return &attachment{client: remote, transport: transport, session: resp.SessionID}
}

// contenoxSessionID resolves the internal session id the router keys on from
// the ACP session id a client holds. acpsvc stores the ACP id as the session's
// name, so the roster is the mapping.
func (h *harness) contenoxSessionID(ctx context.Context, sid libacp.SessionID) string {
	h.t.Helper()
	roster, err := sessionservice.New(h.db, harnessWorkspace, libtracker.NoopTracker{}).List(ctx, acpSessionIdentity)
	require.NoError(h.t, err)
	for _, s := range roster {
		if s.Name == string(sid) {
			require.NotEmpty(h.t, s.ID)
			return s.ID
		}
	}
	h.t.Fatalf("no stored session is named %s", sid)
	return ""
}

// cardWatch is the single consumer of the Bridge's event outlet, recording the
// permission cards that reached the local UI and handing them over so a test
// can answer one. Events() has one consumer by contract, so a test asserting
// on cards has to be that consumer.
type cardWatch struct {
	shown chan PermissionRequested

	mu    sync.Mutex
	count int
}

// watchCards starts draining the outlet. Cards raised before it starts are not
// lost: the queue behind Events is unbounded and drains in order.
//
// The draining goroutine is not joined, and must not be: it ends when Events
// closes, which is the harness's own Close, and a t.Cleanup waiting on it would
// run before that Close and deadlock.
func (h *harness) watchCards() *cardWatch {
	h.t.Helper()
	w := &cardWatch{shown: make(chan PermissionRequested, 8)}
	go func() {
		for ev := range h.bridge.Events() {
			card, ok := ev.(PermissionRequested)
			if !ok {
				continue
			}
			w.mu.Lock()
			w.count++
			w.mu.Unlock()
			select {
			case w.shown <- card:
			default:
			}
		}
	}()
	return w
}

func (w *cardWatch) seen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// askOn raises one approval through the router the way the engine does, on the
// contenox session named by id, and returns the verdict asynchronously so the
// caller can answer the card the ask produces.
func askOn(ctx context.Context, router *acpsvc.SessionRouter, id, toolCallID string) <-chan approvalResult {
	out := make(chan approvalResult, 1)
	go func() {
		allowed, err := router.AskApproval(
			context.WithValue(ctx, runtimetypes.SessionIDContextKey, id),
			hitlservice.ApprovalRequest{ToolCallID: toolCallID, ToolName: "local_fs.write_file"},
		)
		out <- approvalResult{allowed: allowed, err: err}
	}()
	return out
}

type approvalResult struct {
	allowed bool
	err     error
}

// TestUnit_ApprovalRaisedOnAnAttachmentGoesToTheRemoteClient is the defect the
// relay wiring closes for a terminal surface. Before it, the surface bound one
// transport and every approval resolved through it: work started on a phone
// raised its card on the operator's screen, and the phone waited on a question
// it was never shown.
//
// Both halves are asserted. The card arriving remotely is not enough on its
// own — a router that fanned out to every attached client would satisfy it
// while putting one question in front of two humans.
func TestUnit_ApprovalRaisedOnAnAttachmentGoesToTheRemoteClient(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	local := h.initSession(ctx)
	cards := h.watchCards()
	loopback := h.bridge.Transport()
	remote := h.attach(ctx, true)

	require.NotSame(t, loopback, remote.transport, "an attachment must be its own transport")
	require.Same(t, loopback, h.bridge.Transport(),
		"serving an attachment must not replace the transport the surface itself draws")
	require.NotEqual(t, local, remote.session, "the attachment must open its own session")

	select {
	case got := <-askOn(ctx, h.router, h.contenoxSessionID(ctx, remote.session), "call-remote"):
		require.NoError(t, got.err)
		require.True(t, got.allowed, "the verdict must come back from the client that was asked")
	case <-time.After(cardBudget):
		t.Fatal("the approval raised on the attachment was never answered")
	}

	require.Equal(t, 1, remote.client.count(), "the remote client must have been shown the card")
	req, ok := remote.client.last()
	require.True(t, ok)
	require.Equal(t, "call-remote", string(req.ToolCall.ToolCallID))
	require.Equal(t, remote.session, req.SessionID)
	require.Zero(t, cards.seen(), "a card raised by remote work must never reach the local UI")
}

// TestUnit_ApprovalWithNothingAttachedStaysOnTheLocalSurface pins the other
// direction: a runtime nobody has attached to behaves exactly as it did before
// remote attachments existed. The loopback owns its own sessions, so its
// approvals surface as the inline card; a session nobody holds reports
// ErrNoBoundSession, which is the one condition the surface's approval
// callback falls back to its own transport on.
func TestUnit_ApprovalWithNothingAttachedStaysOnTheLocalSurface(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	local := h.initSession(ctx)
	cards := h.watchCards()

	verdict := askOn(ctx, h.router, h.contenoxSessionID(ctx, local), "call-local")
	select {
	case card := <-cards.shown:
		require.Equal(t, "call-local", string(card.ToolCallID))
		require.Equal(t, local, card.SessionID)
		card.Resolve(true)
	case <-time.After(cardBudget):
		t.Fatal("no card reached the local UI")
	}
	select {
	case got := <-verdict:
		require.NoError(t, got.err)
		require.True(t, got.allowed)
	case <-time.After(cardBudget):
		t.Fatal("the operator's verdict never came back")
	}

	_, err := h.router.AskApproval(
		context.WithValue(ctx, runtimetypes.SessionIDContextKey, "no-such-session"),
		hitlservice.ApprovalRequest{ToolCallID: "call-orphan", ToolName: "local_fs.write_file"},
	)
	require.ErrorIs(t, err, acpsvc.ErrNoBoundSession,
		"an unheld session must fall through to the surface's own policy")
}
