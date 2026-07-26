package libacp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientCancelHarness drives a ClientSideConnection directly over the wire,
// playing the role of the agent with full control over raw requests and
// notifications. It mirrors cancelHarness (conn_cancel_test.go) but from the
// opposite side, so ClientSideConnection's cancellation behavior can be
// exercised at the JSON-RPC level without needing a full
// AgentSideConnection.
type clientCancelHarness struct {
	t          *testing.T
	writer     func(v any) error
	reader     func() ([]byte, error)
	agentSide  io.ReadWriteCloser
	runErr     chan error
	clientConn *libacp.ClientSideConnection
}

func newClientCancelHarness(t *testing.T, client libacp.Client) *clientCancelHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	clientSide, agentSide := newPipePair()
	conn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client { return client })
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()
	t.Cleanup(func() { _ = agentSide.Close() })

	return &clientCancelHarness{
		t:          t,
		writer:     bufWriter(agentSide),
		reader:     bufReader(agentSide),
		agentSide:  agentSide,
		runErr:     runErr,
		clientConn: conn,
	}
}

func (h *clientCancelHarness) send(method string, id int64, params any) {
	h.t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(h.t, err)
	require.NoError(h.t, h.writer(libacp.NewRequest(libacp.NewRequestIDNumber(id), method, raw)))
}

func (h *clientCancelHarness) notify(method string, params any) {
	h.t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(h.t, err)
	require.NoError(h.t, h.writer(libacp.NewNotification(method, raw)))
}

func (h *clientCancelHarness) expectResponse(id int64) libacp.Response {
	h.t.Helper()
	line, err := h.reader()
	require.NoError(h.t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(h.t, err)
	require.Equal(h.t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.Equal(h.t, libacp.NewRequestIDNumber(id), in.Response.ID, "wire: %s", line)
	return in.Response
}

// wireGenericConnections is wireUpTestConnections (clientconn_test.go)
// generalized to any libacp.Agent/libacp.Client pair, so tests in this file
// can use agents that need their AgentSideConnection wired back in
// (setConn) without depending on the *testAgent concrete type.
func wireGenericConnections(t *testing.T, ctx context.Context, agentFactory libacp.AgentFactory, clientFactory libacp.ClientFactory) (*libacp.AgentSideConnection, *libacp.ClientSideConnection, func()) {
	t.Helper()

	agentSide, clientSide := newPipePair()
	agentConn := libacp.NewAgentSideConnection(agentSide, agentFactory)
	clientConn := libacp.NewClientSideConnection(clientSide, clientFactory)

	agentRunErr := make(chan error, 1)
	go func() { agentRunErr <- agentConn.Run(ctx) }()
	clientRunErr := make(chan error, 1)
	go func() { clientRunErr <- clientConn.Run(ctx) }()

	cleanup := func() {
		_ = agentSide.Close()
		select {
		case <-agentRunErr:
		case <-time.After(2 * time.Second):
			t.Error("agent connection did not shut down")
		}
		select {
		case <-clientRunErr:
		case <-time.After(2 * time.Second):
			t.Error("client connection did not shut down")
		}
	}
	return agentConn, clientConn, cleanup
}

// blockingReadClient's ReadTextFile parks on its context and returns
// ctx.Err() when cancelled, signalling its start on entered. This models a
// Client whose fs/read_text_file implementation is genuinely slow (e.g. a
// large file) when the agent abandons the request.
type blockingReadClient struct {
	libacp.UnimplementedClient
	entered chan struct{}
}

func (c *blockingReadClient) ReadTextFile(ctx context.Context, _ libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	close(c.entered)
	<-ctx.Done()
	return libacp.ReadTextFileResponse{}, ctx.Err()
}

// "$/cancel_request" aborts the request with that JSON-RPC id on
// ClientSideConnection too — session-agnostic protocol-level cancellation,
// mirroring TestUnit_CancelRequest_AbortsInFlightPromptByRequestID
// (conn_cancel_test.go) on the agent side.
func TestUnit_CancelRequest_AbortsInFlightFSReadByRequestID(t *testing.T) {
	client := &blockingReadClient{entered: make(chan struct{})}
	h := newClientCancelHarness(t, client)

	h.send(libacp.MethodFSReadTextFile, 5, libacp.ReadTextFileRequest{SessionID: "sess-1", Path: "/tmp/x.txt"})
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadTextFile never started")
	}

	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(5)})

	resp := h.expectResponse(5)
	// Unlike session/prompt (which has a spec-mandated "cancelled" stop
	// reason carve-out), fs/read_text_file has no such special response
	// shape: conn.go's callMethod has no per-method translation for it
	// either, so whatever error the handler returns for its cancelled
	// context is wrapped as a plain JSON-RPC error via AsError.
	require.NotNil(t, resp.Error, "wire: cancelling a non-prompt request must still answer it, as an error response")
	assert.Equal(t, libacp.ErrInternalError, resp.Error.Code)

	// Unknown ids are ignored, and the connection stays healthy.
	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(999)})
	h.send(libacp.MethodFSReadTextFile, 6, libacp.ReadTextFileRequest{SessionID: "sess-1", Path: "/tmp/y.txt"})
	// The second read blocks forever on its own ctx (never cancelled); cancel
	// it too so the harness's cleanup doesn't leak a goroutine past the test.
	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(6)})
	resp = h.expectResponse(6)
	require.NotNil(t, resp.Error)
}

// blockingAgent (conn_cancel_test.go) is reused below for outbound-call
// cancellation scenarios that don't need an agent-initiated request.

// outbound Prompt ctx cancelled -> $/cancel_request observed by the agent
// side, Prompt returns ctx.Err(). Mirrors
// TestUnit_AbandonedClientCall_SendsCancelRequest (conn_cancel_test.go) but
// for ClientSideConnection's outbound call.
func TestUnit_ClientSideConnection_PromptCtxCancel_EmitsCancelRequestAndReturnsCtxErr(t *testing.T) {
	client := &testClient{}
	h := newClientCancelHarness(t, client)

	promptCtx, promptCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.clientConn.Prompt(promptCtx, libacp.PromptRequest{
			SessionID: "sess-1",
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("go")},
		})
		done <- err
	}()

	// Observe the outbound session/prompt request; the fake agent never
	// answers it.
	line, err := h.reader()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindRequest, in.Kind)
	require.Equal(t, libacp.MethodSessionPrompt, in.Request.Method)
	promptReqID := in.Request.ID

	promptCancel()

	// The outbound call's $/cancel_request notification is a blocking pipe
	// write (the test harness has no buffering); it must be drained before
	// Prompt's own return can be observed, since call() writes it
	// synchronously right before returning ctx.Err().
	line, err = h.reader()
	require.NoError(t, err)
	in, err = libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindNotification, in.Kind, "wire: %s", line)
	assert.Equal(t, libacp.MethodCancelRequest, in.Notification.Method)
	var p libacp.CancelRequestNotification
	require.NoError(t, json.Unmarshal(in.Notification.Params, &p))
	assert.True(t, p.RequestID.Equal(promptReqID), "the cancel must target the abandoned session/prompt request")

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return after ctx cancel")
	}
}

// permissionTurnAgent's Prompt asks the client for permission and, once that
// resolves, translates a "cancelled" outcome into the turn's own stop reason
// — an agent proactively honoring the cancellation it learns about through
// the permission response, rather than through its own ctx.
type permissionTurnAgent struct {
	libacp.UnimplementedAgent
	conn     *libacp.AgentSideConnection
	started  chan struct{}
	permResp chan libacp.RequestPermissionResponse
}

func (a *permissionTurnAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *permissionTurnAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: "sess-1"}, nil
}

func (a *permissionTurnAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	close(a.started)
	resp, err := a.conn.RequestPermission(ctx, libacp.RequestPermissionRequest{
		SessionID: req.SessionID,
		ToolCall:  libacp.PermissionToolCall{ToolCallID: "tc-1"},
		Options: []libacp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: libacp.PermissionAllowOnce},
		},
	})
	if err != nil {
		// AgentSideConnection.call() (conn.go) races this outbound call's own
		// ctx.Done() (cancelled the instant session/cancel arrives) against
		// the client's real response arriving on the wire; when both are
		// ready together, which one the select picks is unspecified. This is
		// exactly the scenario prompt-turn.mdx warns about: "Agents MUST
		// catch these errors and return the semantically meaningful
		// cancelled stop reason." A cancelled ctx here always means the turn
		// is being cancelled, regardless of which side of the race fired.
		if errors.Is(err, context.Canceled) {
			return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
		}
		return libacp.PromptResponse{}, err
	}
	a.permResp <- resp
	if resp.Outcome.Outcome == libacp.PermissionOutcomeCancelled {
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

// countingPermClient's RequestPermission blocks on its context — modelling a
// UI permission dialog awaiting user input — signalling entered once invoked
// and counting invocations, so tests can assert whether CancelPrompt's forced
// resolution bypassed it (calls == 0) or raced an already-invoked call
// (calls == 1, but its real return value must still lose to the forced one).
type countingPermClient struct {
	libacp.UnimplementedClient
	calls   atomic.Int32
	entered chan struct{}
}

func (c *countingPermClient) RequestPermission(ctx context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	c.calls.Add(1)
	close(c.entered)
	<-ctx.Done()
	return libacp.RequestPermissionResponse{}, ctx.Err()
}

// TestUnit_ClientSideConnection_CancelPrompt_ForceResolvesPendingPermission
// drives ClientSideConnection directly over the wire (clientCancelHarness), so
// it observes exactly what this connection puts on the wire for a pending
// session/request_permission request once CancelPrompt is called — without
// the added, unrelated raciness of a real AgentSideConnection also reacting
// to session/cancel by cancelling its own outbound call's context (see the
// loopback test below for that end-to-end behavior instead).
func TestUnit_ClientSideConnection_CancelPrompt_ForceResolvesPendingPermission(t *testing.T) {
	client := &countingPermClient{entered: make(chan struct{})}
	h := newClientCancelHarness(t, client)

	go func() {
		_, _ = h.clientConn.Prompt(context.Background(), libacp.PromptRequest{
			SessionID: "sess-1",
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the thing")},
		})
	}()

	// Observe the outbound session/prompt request: by the time it is on the
	// wire, Prompt has already registered its promptTurns entry, so a
	// subsequent CancelPrompt for this session is guaranteed to find it.
	line, err := h.reader()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.MethodSessionPrompt, in.Request.Method)

	// Simulate the agent asking for permission mid-turn.
	h.send(libacp.MethodSessionRequestPermission, 100, libacp.RequestPermissionRequest{
		SessionID: "sess-1",
		ToolCall:  libacp.PermissionToolCall{ToolCallID: "tc-1"},
		Options: []libacp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: libacp.PermissionAllowOnce},
		},
	})
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("permission request never reached the client handler")
	}

	// CancelPrompt's forced write (and CancelSession's notify write right
	// after it) are blocking pipe writes with no buffering on the harness
	// side, so it must run concurrently with draining the wire below rather
	// than before it.
	cancelErr := make(chan error, 1)
	go func() { cancelErr <- h.clientConn.CancelPrompt("sess-1") }()

	// forceCancelSessionPermissions writes the permission response before
	// CancelPrompt sends session/cancel, so wire order puts the response
	// first.
	resp := h.expectResponse(100)
	require.Nil(t, resp.Error, "the forced cancellation must be a valid result, never an error response")
	var permResp libacp.RequestPermissionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &permResp))
	assert.Equal(t, libacp.PermissionOutcomeCancelled, permResp.Outcome.Outcome, "the client must resolve the outcome itself, not the application's blocked (and discarded) answer")

	line, err = h.reader()
	require.NoError(t, err)
	in, err = libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindNotification, in.Kind, "wire: %s", line)
	assert.Equal(t, libacp.MethodSessionCancel, in.Notification.Method)

	select {
	case err := <-cancelErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("CancelPrompt did not return")
	}

	assert.Equal(t, int32(1), client.calls.Load(), "the application handler was invoked once (it was already in flight when cancelled) but its own answer must never reach the wire")
}

// Full prompt-turn cancellation loopback, with a real AgentSideConnection on
// the other end: the client prompts, the agent requests permission, the
// client cancels the turn while that permission request is still pending,
// and the agent resolves session/prompt with stopReason "cancelled" — which
// Prompt returns. permissionTurnAgent's error handling (above) accounts for
// conn.go's own inherent, pre-existing race between an outbound call's ctx
// cancellation and a legitimate response arriving at nearly the same time;
// TestUnit_ClientSideConnection_CancelPrompt_ForceResolvesPendingPermission
// above is the deterministic test of this connection's own contribution to
// that behavior.
func TestUnit_FullPromptTurnCancellation_PendingPermissionAutoResolved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent := &permissionTurnAgent{started: make(chan struct{}), permResp: make(chan libacp.RequestPermissionResponse, 1)}
	client := &countingPermClient{entered: make(chan struct{})}
	_, clientConn, cleanup := wireGenericConnections(t, ctx,
		func(c *libacp.AgentSideConnection) libacp.Agent { agent.conn = c; return agent },
		func(*libacp.ClientSideConnection) libacp.Client { return client },
	)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newSess, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	done := make(chan struct {
		resp libacp.PromptResponse
		err  error
	}, 1)
	go func() {
		resp, err := clientConn.Prompt(ctx, libacp.PromptRequest{
			SessionID: newSess.SessionID,
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the thing")},
		})
		done <- struct {
			resp libacp.PromptResponse
			err  error
		}{resp, err}
	}()

	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never started")
	}
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("permission request never reached the client handler")
	}

	// The permission request is now genuinely pending (blocked on user
	// input). Cancel the turn.
	require.NoError(t, clientConn.CancelPrompt(newSess.SessionID))

	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Equal(t, libacp.StopReasonCancelled, result.resp.StopReason)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not resolve after the turn was cancelled")
	}

	assert.Equal(t, int32(1), client.calls.Load(), "the application handler was invoked once (it was already in flight when cancelled) but its own answer must never reach the wire")
}

// gatedPermissionAgent only asks for permission once told to proceed, so a
// test can cancel the turn strictly before the permission request is even
// sent — the "new request" half of the pending-permission rule.
type gatedPermissionAgent struct {
	libacp.UnimplementedAgent
	conn     *libacp.AgentSideConnection
	started  chan struct{}
	proceed  chan struct{}
	permResp chan libacp.RequestPermissionResponse
}

func (a *gatedPermissionAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *gatedPermissionAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: "sess-1"}, nil
}

func (a *gatedPermissionAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	close(a.started)
	<-a.proceed
	// Spec: the Agent MAY still send updates (and, by extension, still have
	// in-flight tool activity) after session/cancel, as long as it resolves
	// before responding to session/prompt. Using a fresh context here models
	// exactly that: this permission request is sent after the turn's own ctx
	// is already cancelled, deliberately not tied to it, so the request
	// reaches the wire (and the client) rather than being aborted locally by
	// the agent's own outbound-call cancellation (conn.go).
	resp, err := a.conn.RequestPermission(context.Background(), libacp.RequestPermissionRequest{
		SessionID: req.SessionID,
		ToolCall:  libacp.PermissionToolCall{ToolCallID: "tc-1"},
		Options: []libacp.PermissionOption{
			{OptionID: "allow", Name: "Allow", Kind: libacp.PermissionAllowOnce},
		},
	})
	if err != nil {
		return libacp.PromptResponse{}, err
	}
	a.permResp <- resp
	if resp.Outcome.Outcome == libacp.PermissionOutcomeCancelled {
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func TestUnit_CancelPrompt_AutoResolvesNewPermissionRequest_WithoutInvokingHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent := &gatedPermissionAgent{
		started:  make(chan struct{}),
		proceed:  make(chan struct{}),
		permResp: make(chan libacp.RequestPermissionResponse, 1),
	}
	client := &countingPermClient{entered: make(chan struct{})}
	_, clientConn, cleanup := wireGenericConnections(t, ctx,
		func(c *libacp.AgentSideConnection) libacp.Agent { agent.conn = c; return agent },
		func(*libacp.ClientSideConnection) libacp.Client { return client },
	)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newSess, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	done := make(chan struct {
		resp libacp.PromptResponse
		err  error
	}, 1)
	go func() {
		resp, err := clientConn.Prompt(ctx, libacp.PromptRequest{
			SessionID: newSess.SessionID,
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the thing")},
		})
		done <- struct {
			resp libacp.PromptResponse
			err  error
		}{resp, err}
	}()

	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never started")
	}

	// Cancel the turn before the agent has even asked for permission.
	require.NoError(t, clientConn.CancelPrompt(newSess.SessionID))
	close(agent.proceed)

	select {
	case resp := <-agent.permResp:
		assert.Equal(t, libacp.PermissionOutcomeCancelled, resp.Outcome.Outcome)
	case <-time.After(2 * time.Second):
		t.Fatal("agent never received a permission response")
	}

	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Equal(t, libacp.StopReasonCancelled, result.resp.StopReason)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not resolve after the turn was cancelled")
	}

	assert.Equal(t, int32(0), client.calls.Load(), "a permission request arriving after CancelPrompt must never reach the application handler")
}

// stressAgent/stressClient exercise inbound and outbound cancellation
// concurrently: the agent randomly asks for permission (an inbound request to
// the client that may be cancelled from either side, and may itself be
// abandoned if the agent's own ctx dies), and the turn itself may time out or
// be cancelled by the test.
type stressAgent struct {
	libacp.UnimplementedAgent
	conn *libacp.AgentSideConnection
}

func (a *stressAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *stressAgent) NewSession(_ context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: libacp.SessionID(req.Cwd)}, nil
}

func (a *stressAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	if rand.Intn(2) == 0 {
		resp, err := a.conn.RequestPermission(ctx, libacp.RequestPermissionRequest{
			SessionID: req.SessionID,
			ToolCall:  libacp.PermissionToolCall{ToolCallID: "tc"},
			Options: []libacp.PermissionOption{
				{OptionID: "allow", Name: "Allow", Kind: libacp.PermissionAllowOnce},
			},
		})
		if err != nil {
			// In this synthetic scenario every error RequestPermission can
			// produce traces back to cancellation somewhere in the chain —
			// either observed directly (ctx.Err()) or, once the client's own
			// cancelled-ctx error has crossed the wire as a JSON-RPC error
			// response, wrapped as a *libacp.Error whose message says
			// "context canceled" but which errors.Is no longer recognizes as
			// context.Canceled. A real agent facing a genuine (non-
			// cancellation) permission error would propagate it; here there
			// is no such case, so it always resolves the turn as cancelled.
			return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
		}
		if resp.Outcome.Outcome == libacp.PermissionOutcomeCancelled {
			return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
		}
	}
	select {
	case <-ctx.Done():
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	case <-time.After(time.Duration(rand.Intn(3)) * time.Millisecond):
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

type stressClient struct {
	libacp.UnimplementedClient
}

func (c *stressClient) RequestPermission(ctx context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	select {
	case <-ctx.Done():
		return libacp.RequestPermissionResponse{}, ctx.Err()
	case <-time.After(time.Duration(rand.Intn(3)) * time.Millisecond):
		return libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: "allow"},
		}, nil
	}
}

// TestUnit_ConcurrentCancellationStress hammers the same ClientSideConnection
// with many goroutines issuing overlapping outbound calls, inbound
// agent-initiated requests, and random cancellations (ctx cancel, ctx
// timeout, CancelPrompt), meant to be run with -race: nothing here asserts a
// specific outcome per call beyond "no panic, no unexpected error, no hang".
func TestUnit_ConcurrentCancellationStress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	agent := &stressAgent{}
	client := &stressClient{}
	_, clientConn, cleanup := wireGenericConnections(t, ctx,
		func(c *libacp.AgentSideConnection) libacp.Agent { agent.conn = c; return agent },
		func(*libacp.ClientSideConnection) libacp.Client { return client },
	)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	const goroutines = 20
	const itersPerGoroutine = 5

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < itersPerGoroutine; j++ {
				newSess, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{
					Cwd:        fmt.Sprintf("/tmp/sess-%d-%d", i, j),
					McpServers: []libacp.McpServer{},
				})
				if err != nil {
					t.Errorf("NewSession: %v", err)
					return
				}

				var promptCtx context.Context
				var promptCancel context.CancelFunc
				if rand.Intn(2) == 0 {
					promptCtx, promptCancel = context.WithTimeout(ctx, time.Duration(rand.Intn(4))*time.Millisecond)
				} else {
					promptCtx, promptCancel = context.WithCancel(ctx)
				}

				if rand.Intn(2) == 0 {
					go func() {
						time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
						_ = clientConn.CancelPrompt(newSess.SessionID)
					}()
				}
				if rand.Intn(3) == 0 {
					go func() {
						time.Sleep(time.Duration(rand.Intn(2)) * time.Millisecond)
						promptCancel()
					}()
				}

				_, err = clientConn.Prompt(promptCtx, libacp.PromptRequest{
					SessionID: newSess.SessionID,
					Prompt:    []libacp.ContentBlock{libacp.NewTextContent("go")},
				})
				promptCancel()
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Prompt: unexpected error %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
}

// closeAwareClient signals cancelled once its in-flight ReadTextFile handler
// observes ctx.Done(), so a test can confirm connection shutdown propagates
// to already-running handlers.
type closeAwareClient struct {
	libacp.UnimplementedClient
	entered   chan struct{}
	cancelled chan struct{}
}

func (c *closeAwareClient) ReadTextFile(ctx context.Context, _ libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	close(c.entered)
	<-ctx.Done()
	close(c.cancelled)
	return libacp.ReadTextFileResponse{}, ctx.Err()
}

// Connection close mid-request: a pending outbound call fails fast with
// ErrConnectionClosed, and an in-flight inbound handler's context is
// cancelled — mirroring conn.go's shutdown() behavior on the agent side.
func TestUnit_ConnectionClose_FailsPendingCallsAndCancelsHandlers(t *testing.T) {
	client := &closeAwareClient{entered: make(chan struct{}), cancelled: make(chan struct{})}
	h := newClientCancelHarness(t, client)

	h.send(libacp.MethodFSReadTextFile, 1, libacp.ReadTextFileRequest{SessionID: "sess-1", Path: "/tmp/x.txt"})
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadTextFile handler never started")
	}

	promptDone := make(chan error, 1)
	go func() {
		_, err := h.clientConn.Prompt(context.Background(), libacp.PromptRequest{
			SessionID: "sess-1",
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("go")},
		})
		promptDone <- err
	}()

	// Read the outbound session/prompt request off the wire so we know it is
	// registered as pending before closing the connection out from under it.
	line, err := h.reader()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.MethodSessionPrompt, in.Request.Method)

	require.NoError(t, h.agentSide.Close())

	select {
	case err := <-promptDone:
		require.ErrorIs(t, err, libacp.ErrConnectionClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not fail fast after connection close")
	}

	select {
	case <-client.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight handler ctx was not cancelled on connection close")
	}
}
