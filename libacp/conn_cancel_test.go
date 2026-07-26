package libacp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingAgent's Prompt parks on its context and returns ctx.Err() when
// cancelled, signalling each start on `started`. This models an agent whose
// turn only ends via session/cancel.
type blockingAgent struct {
	libacp.UnimplementedAgent
	started chan libacp.SessionID
}

func (a *blockingAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *blockingAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	return libacp.NewSessionResponse{SessionID: "sess-1"}, nil
}

func (a *blockingAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	a.started <- req.SessionID
	<-ctx.Done()
	return libacp.PromptResponse{}, ctx.Err()
}

func (a *blockingAgent) ListSessions(_ context.Context, _ libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	return libacp.ListSessionsResponse{Sessions: []libacp.SessionInfo{}}, nil
}

type cancelHarness struct {
	t          *testing.T
	writer     func(v any) error
	reader     func() ([]byte, error)
	clientSide io.ReadWriteCloser
	runErr     chan error
}

func newCancelHarness(t *testing.T, agent libacp.Agent) *cancelHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	agentSide, clientSide := newPipePair()
	conn := libacp.NewAgentSideConnection(agentSide, func(*libacp.AgentSideConnection) libacp.Agent { return agent })
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()
	t.Cleanup(func() { _ = clientSide.Close() })

	return &cancelHarness{
		t:          t,
		writer:     bufWriter(clientSide),
		reader:     bufReader(clientSide),
		clientSide: clientSide,
		runErr:     runErr,
	}
}

func (h *cancelHarness) send(method string, id int64, params any) {
	h.t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(h.t, err)
	require.NoError(h.t, h.writer(libacp.NewRequest(libacp.NewRequestIDNumber(id), method, raw)))
}

func (h *cancelHarness) notify(method string, params any) {
	h.t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(h.t, err)
	require.NoError(h.t, h.writer(libacp.NewNotification(method, raw)))
}

func (h *cancelHarness) expectResponse(id int64) libacp.Response {
	h.t.Helper()
	line, err := h.reader()
	require.NoError(h.t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(h.t, err)
	require.Equal(h.t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
	require.Equal(h.t, libacp.NewRequestIDNumber(id), in.Response.ID, "wire: %s", line)
	return in.Response
}

func promptParams(session string) libacp.PromptRequest {
	return libacp.PromptRequest{
		SessionID: libacp.SessionID(session),
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("go")},
	}
}

func requireCancelledStop(t *testing.T, resp libacp.Response) {
	t.Helper()
	require.Nil(t, resp.Error, "cancellation must not surface as a JSON-RPC error")
	var pr libacp.PromptResponse
	require.NoError(t, json.Unmarshal(resp.Result, &pr))
	assert.Equal(t, libacp.StopReasonCancelled, pr.StopReason)
}

func TestUnit_SessionCancel_CancelsInFlightPrompt(t *testing.T) {
	agent := &blockingAgent{started: make(chan libacp.SessionID, 2)}
	h := newCancelHarness(t, agent)

	h.send(libacp.MethodSessionPrompt, 1, promptParams("sess-1"))
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never started")
	}

	h.notify(libacp.MethodSessionCancel, libacp.CancelNotification{SessionID: "sess-1"})
	requireCancelledStop(t, h.expectResponse(1))
}

// Regression: the cleanup guard used to compare cancel funcs via %p, which is
// always true for closures of the same function — the first prompt's cleanup
// deleted the second prompt's registration, so session/cancel found nothing
// and the second turn ran forever.
func TestUnit_OverlappingPrompts_CancelStillReachesSecondPrompt(t *testing.T) {
	agent := &blockingAgent{started: make(chan libacp.SessionID, 2)}
	h := newCancelHarness(t, agent)

	h.send(libacp.MethodSessionPrompt, 1, promptParams("sess-1"))
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first prompt never started")
	}

	// Second prompt on the same session supersedes the first: its registration
	// cancels prompt #1, which resolves as cancelled.
	h.send(libacp.MethodSessionPrompt, 2, promptParams("sess-1"))
	requireCancelledStop(t, h.expectResponse(1))
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second prompt never started")
	}

	// Prompt #1's cleanup has run by now (its response is on the wire). The
	// registry must still hold prompt #2, so this cancel must end it.
	h.notify(libacp.MethodSessionCancel, libacp.CancelNotification{SessionID: "sess-1"})
	requireCancelledStop(t, h.expectResponse(2))
}

// Wire order is authoritative: a cancel written immediately after its prompt —
// before the prompt handler goroutine has done anything — must still cancel
// that prompt, because registration happens at dispatch time on the read loop.
func TestUnit_CancelImmediatelyAfterPrompt_PreservesWireOrder(t *testing.T) {
	agent := &blockingAgent{started: make(chan libacp.SessionID, 2)}
	h := newCancelHarness(t, agent)

	h.send(libacp.MethodSessionPrompt, 1, promptParams("sess-1"))
	h.notify(libacp.MethodSessionCancel, libacp.CancelNotification{SessionID: "sess-1"})

	requireCancelledStop(t, h.expectResponse(1))
}

// $/cancel_request aborts the request with that JSON-RPC id — session-agnostic
// protocol-level cancellation, wire-ordered like session/cancel.
func TestUnit_CancelRequest_AbortsInFlightPromptByRequestID(t *testing.T) {
	agent := &blockingAgent{started: make(chan libacp.SessionID, 2)}
	h := newCancelHarness(t, agent)

	h.send(libacp.MethodSessionPrompt, 7, promptParams("sess-1"))
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never started")
	}

	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(7)})
	requireCancelledStop(t, h.expectResponse(7))

	// Unknown ids are ignored, and the connection stays healthy.
	h.notify(libacp.MethodCancelRequest, libacp.CancelRequestNotification{RequestID: libacp.NewRequestIDNumber(999)})
	h.send(libacp.MethodInitialize, 8, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	resp := h.expectResponse(8)
	require.Nil(t, resp.Error)
}

// permissionAgent's Prompt blocks on an agent→client RequestPermission call;
// used to observe outbound $/cancel_request when the turn dies mid-approval.
type permissionAgent struct {
	libacp.UnimplementedAgent
	conn    *libacp.AgentSideConnection
	started chan struct{}
}

func (a *permissionAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (a *permissionAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	close(a.started)
	_, err := a.conn.RequestPermission(ctx, libacp.RequestPermissionRequest{SessionID: req.SessionID})
	if err != nil {
		return libacp.PromptResponse{}, err
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func TestUnit_AbandonedClientCall_SendsCancelRequest(t *testing.T) {
	agent := &permissionAgent{started: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent.conn = c
		return agent
	})
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()
	defer clientSide.Close()

	h := &cancelHarness{t: t, writer: bufWriter(clientSide), reader: bufReader(clientSide), clientSide: clientSide, runErr: runErr}

	h.send(libacp.MethodSessionPrompt, 1, promptParams("sess-1"))
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never started")
	}

	// The client sees the permission request but never answers it; instead it
	// cancels the turn. The agent must abandon the awaited call AND tell the
	// client via $/cancel_request so the dialog can be torn down.
	line, err := h.reader()
	require.NoError(t, err)
	in, err := libacp.ParseIncoming(line)
	require.NoError(t, err)
	require.Equal(t, libacp.IncomingKindRequest, in.Kind)
	require.Equal(t, libacp.MethodSessionRequestPermission, in.Request.Method)
	permReqID := in.Request.ID

	h.notify(libacp.MethodSessionCancel, libacp.CancelNotification{SessionID: "sess-1"})

	var sawCancelRequest bool
	for i := 0; i < 4; i++ {
		line, err := h.reader()
		require.NoError(t, err)
		in, err := libacp.ParseIncoming(line)
		require.NoError(t, err)
		if in.Kind == libacp.IncomingKindNotification && in.Notification.Method == libacp.MethodCancelRequest {
			var p libacp.CancelRequestNotification
			require.NoError(t, json.Unmarshal(in.Notification.Params, &p))
			require.True(t, p.RequestID.Equal(permReqID), "the cancel must target the abandoned permission request")
			sawCancelRequest = true
			continue
		}
		if in.Kind == libacp.IncomingKindResponse {
			requireCancelledStop(t, in.Response)
			break
		}
	}
	require.True(t, sawCancelRequest, "abandoning an awaited client call must emit $/cancel_request")
}

func TestUnit_MalformedInput_GetsJSONRPCErrorResponses(t *testing.T) {
	agent := &blockingAgent{started: make(chan libacp.SessionID, 1)}
	h := newCancelHarness(t, agent)

	expectError := func(code int) libacp.Response {
		t.Helper()
		line, err := h.reader()
		require.NoError(t, err)
		in, err := libacp.ParseIncoming(line)
		require.NoError(t, err)
		require.Equal(t, libacp.IncomingKindResponse, in.Kind, "wire: %s", line)
		require.NotNil(t, in.Response.Error, "wire: %s", line)
		assert.Equal(t, code, in.Response.Error.Code)
		return in.Response
	}

	// Invalid JSON: parse error with null id.
	_, err := h.clientSide.Write([]byte("this is not json\n"))
	require.NoError(t, err)
	resp := expectError(libacp.ErrParseError)
	assert.Equal(t, libacp.NewRequestIDNull(), resp.ID)

	// Valid JSON, invalid JSON-RPC (boolean id): invalid request, id null
	// because a boolean id cannot be salvaged.
	_, err = h.clientSide.Write([]byte(`{"jsonrpc":"2.0","id":true,"method":"initialize"}` + "\n"))
	require.NoError(t, err)
	expectError(libacp.ErrInvalidRequest)

	// Valid JSON, neither method nor id.
	_, err = h.clientSide.Write([]byte(`{"jsonrpc":"2.0"}` + "\n"))
	require.NoError(t, err)
	expectError(libacp.ErrInvalidRequest)

	// The connection survives malformed input: a normal request still works.
	h.send(libacp.MethodInitialize, 9, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	resp = h.expectResponse(9)
	require.Nil(t, resp.Error)
}

func TestUnit_AbsentParams_TreatedAsEmptyObject(t *testing.T) {
	agent := &blockingAgent{started: make(chan libacp.SessionID, 1)}
	h := newCancelHarness(t, agent)

	// session/list has all-optional params; omitting them entirely must work.
	require.NoError(t, h.writer(libacp.NewRequest(libacp.NewRequestIDNumber(1), libacp.MethodSessionList, nil)))
	resp := h.expectResponse(1)
	require.Nil(t, resp.Error)
	var list libacp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(resp.Result, &list))
}

type panickyAgent struct {
	libacp.UnimplementedAgent
}

func (panickyAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func (panickyAgent) NewSession(_ context.Context, _ libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	panic("boom")
}

func TestUnit_HandlerPanic_BecomesInternalErrorResponse(t *testing.T) {
	h := newCancelHarness(t, panickyAgent{})

	h.send(libacp.MethodSessionNew, 1, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	resp := h.expectResponse(1)
	require.NotNil(t, resp.Error)
	assert.Equal(t, libacp.ErrInternalError, resp.Error.Code)

	// The connection survives the panic.
	h.send(libacp.MethodInitialize, 2, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	resp = h.expectResponse(2)
	require.Nil(t, resp.Error)
}

func TestUnit_RunReturnsContextError_OnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agentSide, clientSide := newPipePair()
	defer clientSide.Close()

	conn := libacp.NewAgentSideConnection(agentSide, func(*libacp.AgentSideConnection) libacp.Agent {
		return &blockingAgent{started: make(chan libacp.SessionID, 1)}
	})
	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	cancel()
	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled, "Run must surface the cancellation, not the transport teardown error")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

func ExampleSessionConfigOptionValue() {
	var req libacp.SetSessionConfigOptionRequest
	_ = json.Unmarshal([]byte(`{"sessionId":"s","configId":"c","value":"model-x"}`), &req)
	fmt.Println(req.Value.AsString(), req.Value.IsBool)

	_ = json.Unmarshal([]byte(`{"sessionId":"s","configId":"c","type":"boolean","value":true}`), &req)
	fmt.Println(req.Value.AsString(), req.Value.IsBool)
	// Output:
	// model-x false
	// true true
}
