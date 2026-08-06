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

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientCancelHarness drives a ClientSideConnection directly over the wire,
// playing the agent role with raw requests/notifications (mirrors
// cancelHarness in conn_cancel_test.go, from the opposite side).
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

// wireGenericConnections generalizes wireUpTestConnections (clientconn_test.go)
// to any libacp.Agent/libacp.Client pair.
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

// blockingReadClient models a slow fs/read_text_file: ReadTextFile parks on
// its context and returns ctx.Err() when cancelled, signalling start on entered.
type blockingReadClient struct {
	libacp.UnimplementedClient
	entered chan struct{}
}

func (c *blockingReadClient) ReadTextFile(ctx context.Context, _ libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	close(c.entered)
	<-ctx.Done()
	return libacp.ReadTextFileResponse{}, ctx.Err()
}

// Invariant: "$/cancel_request" aborts the request with that JSON-RPC id on
// ClientSideConnection too, session-agnostic (mirrors
// TestUnit_CancelRequest_AbortsInFlightPromptByRequestID on the agent side).
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
	// Unlike session/prompt, fs/read_text_file has no "cancelled" carve-out:
	// its cancelled-context error is wrapped as a plain JSON-RPC error.
	require.NotNil(t, resp.Error, "wire: cancelling a non-prompt request must still answer it, as an error response")
	assert.Equal(t, libacp.ErrInternalError, resp.Error.Code)

	// Unknown ids are ignored, and the connection stays healthy.
	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(999)})
	h.send(libacp.MethodFSReadTextFile, 6, libacp.ReadTextFileRequest{SessionID: "sess-1", Path: "/tmp/y.txt"})
	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(6)})
	resp = h.expectResponse(6)
	require.NotNil(t, resp.Error)
}

// Invariant: cancelling an outbound Prompt's ctx emits "$/cancel_request" for
// the abandoned request and returns ctx.Err() (mirrors
// TestUnit_AbandonedClientCall_SendsCancelRequest for the outbound call).
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

	// Observe the outbound session/prompt request; the fake agent never answers it.
	line, err := h.reader()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindRequest, in.Kind)
	require.Equal(t, libacp.MethodSessionPrompt, in.Request.Method)
	promptReqID := in.Request.ID

	promptCancel()

	// call() writes $/cancel_request synchronously before returning ctx.Err(),
	// so it must be drained before Prompt's own return is observed.
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

// permissionTurnAgent's Prompt asks the client for permission and translates
// a "cancelled" outcome into the turn's own stop reason.
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
		// call() races this outbound call's ctx.Done() against the client's
		// real response; a cancelled ctx here always means the turn is being
		// cancelled, per the spec's "catch and return cancelled" contract.
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

// countingPermClient's RequestPermission blocks on its context, modelling a UI
// permission dialog, and counts invocations so tests can assert whether
// CancelPrompt's forced resolution bypassed it or raced an already-invoked call.
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

// Invariant: CancelPrompt force-resolves a pending session/request_permission
// request as "cancelled" on the wire, observed here without a real
// AgentSideConnection's added raciness (see the loopback test below for that).
func TestUnit_ClientSideConnection_CancelPrompt_ForceResolvesPendingPermission(t *testing.T) {
	client := &countingPermClient{entered: make(chan struct{})}
	h := newClientCancelHarness(t, client)

	go func() {
		_, _ = h.clientConn.Prompt(context.Background(), libacp.PromptRequest{
			SessionID: "sess-1",
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the thing")},
		})
	}()

	// By the time this request is on the wire, Prompt has already registered
	// its promptTurns entry, so a subsequent CancelPrompt is guaranteed to find it.
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

	// Blocking pipe writes with no harness buffering, so run concurrently
	// with draining the wire below.
	cancelErr := make(chan error, 1)
	go func() { cancelErr <- h.clientConn.CancelPrompt("sess-1") }()

	// forceCancelSessionPermissions writes before CancelPrompt's session/cancel.
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

// Invariant: end-to-end with a real AgentSideConnection, cancelling a turn
// while its permission request is pending resolves session/prompt with
// stopReason "cancelled".
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

	// The permission request is now genuinely pending; cancel the turn.
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
// test can cancel the turn strictly before the request is sent.
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
	// Spec: the agent may still have in-flight activity after session/cancel
	// as long as it resolves before responding. A fresh, untied context here
	// models that, so the request reaches the wire instead of being aborted
	// locally by the turn's own cancelled context.
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
// concurrently: random permission requests and turn timeouts/cancellations.
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
			// Every error here traces back to cancellation somewhere in the
			// chain, so it always resolves the turn as cancelled.
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

// Invariant (race-detector target): overlapping outbound calls, inbound
// requests, and random cancellations never panic, hang, or produce an
// unexpected error.
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
// observes ctx.Done(), confirming shutdown propagates to running handlers.
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

// Invariant: closing the connection mid-request fails a pending outbound
// call fast with ErrConnectionClosed and cancels an in-flight inbound
// handler's context (mirrors conn.go's shutdown on the agent side).
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

	// Confirm the request is registered as pending before closing the connection.
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
