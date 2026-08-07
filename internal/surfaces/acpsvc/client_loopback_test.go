package acpsvc

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// This file drives the real production acpsvc Transport through a real
// libacp.ClientSideConnection over an in-memory duplex pipe, both Run loops
// live — proving the two libacp halves interoperate, not re-testing either
// in isolation. Deps are mocked (sessionEntry.Agent swapped for a scripted
// double after a real session/new), but the event bus uses the SQLite
// backend serve wires, not libbus.NewInMem(): prompt.go's post-turn
// `sub.Unsubscribe(); close(rawCh); <-translateDone` only reliably drains
// trailing events on a backend that hands over what was published before
// Unsubscribe (a libbus conformance property that is SQLite-only); on InMem
// the queued events race the teardown and can be lost under load.

// loopbackClient is a minimal libacp.Client that answers the agent's reverse
// calls (session/request_permission, fs/*) deterministically instead of
// prompting a human, and buffers every session/update notification in wire
// order so tests can assert on the stream a real editor would render.
type loopbackClient struct {
	libacp.UnimplementedClient

	updates chan libacp.SessionNotification

	mu    sync.Mutex
	files map[string]string

	permMu   sync.Mutex
	permReqs []libacp.RequestPermissionRequest
	permResp libacp.RequestPermissionResponse
}

func newLoopbackClient() *loopbackClient {
	return &loopbackClient{
		updates: make(chan libacp.SessionNotification, 256),
		files:   make(map[string]string),
		permResp: libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{
				Outcome:  libacp.PermissionOutcomeSelected,
				OptionID: approvalflow.OptionAllow,
			},
		},
	}
}

func (c *loopbackClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.updates <- n
	return nil
}

func (c *loopbackClient) RequestPermission(_ context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	c.permMu.Lock()
	c.permReqs = append(c.permReqs, req)
	resp := c.permResp
	c.permMu.Unlock()
	return resp, nil
}

func (c *loopbackClient) setPermissionResponse(resp libacp.RequestPermissionResponse) {
	c.permMu.Lock()
	c.permResp = resp
	c.permMu.Unlock()
}

func (c *loopbackClient) lastPermissionRequest() (libacp.RequestPermissionRequest, bool) {
	c.permMu.Lock()
	defer c.permMu.Unlock()
	if len(c.permReqs) == 0 {
		return libacp.RequestPermissionRequest{}, false
	}
	return c.permReqs[len(c.permReqs)-1], true
}

func (c *loopbackClient) ReadTextFile(_ context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.files[req.Path]
	if !ok {
		return libacp.ReadTextFileResponse{}, &libacp.Error{Code: libacp.ErrResourceNotFound, Message: "no such file: " + req.Path}
	}
	return libacp.ReadTextFileResponse{Content: content}, nil
}

func (c *loopbackClient) WriteTextFile(_ context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error) {
	c.mu.Lock()
	c.files[req.Path] = req.Content
	c.mu.Unlock()
	return libacp.WriteTextFileResponse{}, nil
}

// drain reads exactly n session/update notifications, in wire order, failing
// the test if they don't arrive within the deadline.
func (c *loopbackClient) drain(t *testing.T, n int) []libacp.SessionNotification {
	t.Helper()
	got := make([]libacp.SessionNotification, 0, n)
	deadline := time.After(30 * time.Second)
	for len(got) < n {
		select {
		case note := <-c.updates:
			got = append(got, note)
		case <-deadline:
			t.Fatalf("timed out waiting for %d session/update notifications (got %d: %+v)", n, len(got), got)
		}
	}
	return got
}

// loopbackAgent is an agentservice.Agent double whose Prompt each test
// scripts directly; every other method is a no-op since session lifecycle
// runs through the real agentservice.Agent NewSession already wired up.
type loopbackAgent struct {
	promptFunc func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error)
}

func (a *loopbackAgent) Capabilities(context.Context) (*agentservice.AgentCapabilities, error) {
	return nil, nil
}
func (a *loopbackAgent) SessionNew(context.Context, string) (string, error) { return "", nil }
func (a *loopbackAgent) SessionList(context.Context) ([]*agentservice.SessionInfo, error) {
	return nil, nil
}
func (a *loopbackAgent) SessionLoad(context.Context, string) (string, []taskengine.Message, error) {
	return "", nil, nil
}
func (a *loopbackAgent) SessionResume(context.Context, string) (string, error) { return "", nil }
func (a *loopbackAgent) SessionDelete(context.Context, string) error           { return nil }
func (a *loopbackAgent) SessionEnsureDefault(context.Context) (string, error)  { return "", nil }
func (a *loopbackAgent) Prompt(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	return a.promptFunc(ctx, req)
}

var _ agentservice.Agent = (*loopbackAgent)(nil)

// loopbackHarness wires the real production Transport (New(Deps)) to a real
// libacp.ClientSideConnection over an in-memory duplex pipe, both Run loops
// live.
type loopbackHarness struct {
	t      *testing.T
	tr     *Transport
	client *libacp.ClientSideConnection
	lc     *loopbackClient
	bus    libbus.Messenger
	router *SessionRouter
}

func newLoopbackHarness(t *testing.T) *loopbackHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "loopback.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	// Poll interval shortened only so events surface promptly; the delivery
	// guarantee this harness relies on (Unsubscribe's final drain) doesn't
	// depend on the tick. See the file header.
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{
		EventPoll:   5 * time.Millisecond,
		RequestPoll: 5 * time.Millisecond,
	})
	// Mirrors serve's shared SessionRouter for HITL approval routing; inert
	// for tests that never consult it.
	router := NewSessionRouter()
	factory := New(Deps{
		Engine:        &enginesvc.Engine{Bus: bus},
		DB:            db,
		ChainRegistry: &ChainRegistry{defaultChain: &taskengine.TaskChainDefinition{}},
		WorkspaceID:   "loopback-ws",
		SessionRouter: router,
	})

	var tr *Transport
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		a := factory(c)
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

	t.Cleanup(func() {
		cancel()
		select {
		case <-agentDone:
		case <-time.After(2 * time.Second):
			t.Error("agent connection did not shut down")
		}
		select {
		case <-clientDone:
		case <-time.After(2 * time.Second):
			t.Error("client connection did not shut down")
		}
		// Before the DB: the bus's cleanup goroutine queries it.
		require.NoError(t, bus.Close())
		require.NoError(t, db.Close())
	})

	return &loopbackHarness{t: t, tr: tr, client: clientConn, lc: lc, bus: bus, router: router}
}

// swapAgent installs a into sid's live sessionEntry, replacing the real
// agentservice.Agent NewSession created, for the test's Prompt calls.
func (h *loopbackHarness) swapAgent(sid libacp.SessionID, a agentservice.Agent) {
	h.tr.sessionMu.Lock()
	h.tr.sessions[sid].driver.(*nativeDriver).agent = a
	h.tr.sessionMu.Unlock()
}

// TestLoopback_InitializeAdvertisesSpecCapabilities pins: implemented session capabilities are advertised; unimplemented additionalDirectories is not.
func TestLoopback_InitializeAdvertisesSpecCapabilities(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	resp, err := h.client.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "loopback-test", Version: "0"},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.ProtocolVersion, resp.ProtocolVersion)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.List)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Resume)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Close)
	require.NotNil(t, resp.AgentCapabilities.SessionCapabilities.Delete)
	require.Nil(t, resp.AgentCapabilities.SessionCapabilities.AdditionalDirectories,
		"acpsvc never reads NewSessionRequest.AdditionalDirectories (session.go), so advertising support would be dishonest")
}

// TestUnit_Initialize_DoesNotAdvertiseAdditionalDirectories is the fast,
// wire-free companion to the loopback check above: it pins the same
// capability-honesty verdict directly against Transport.Initialize.
func TestUnit_Initialize_DoesNotAdvertiseAdditionalDirectories(t *testing.T) {
	tr := &Transport{deps: Deps{Engine: &enginesvc.Engine{}}}
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	require.Nil(t, resp.AgentCapabilities.SessionCapabilities.AdditionalDirectories)
}

// TestLoopback_Prompt_StreamsUpdatesThroughRealClient pins: a streamed turn's chunk, tool-call pending/completed pair, and usage update all reach the real client, alongside session_info_update.
//
// Ordering note: session_info_update is not last on the wire — session/prompt's
// per-turn context doesn't carry an after-response sink, so AfterResponse
// runs synchronously before the turn's own streamed events flush. Existing,
// already-shipped behavior; asserted by kind below, not position.
func TestLoopback_Prompt_StreamsUpdatesThroughRealClient(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.SessionID)
	h.lc.drain(t, 1) // deferred available_commands_update

	fake := &loopbackAgent{}
	fake.promptFunc = func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		reqID, _ := ctx.Value(libtracker.ContextKeyRequestID).(string)
		require.NotEmpty(t, reqID, "acpsvc stamps a request id onto the prompt ctx before calling the agent (prompt.go)")
		subject := taskengine.TaskEventRequestSubject(reqID)
		publish := func(ev taskengine.TaskEvent) {
			raw, mErr := json.Marshal(ev)
			require.NoError(t, mErr)
			require.NoError(t, h.bus.Publish(ctx, subject, raw))
		}
		publish(taskengine.TaskEvent{Kind: taskengine.TaskEventStepChunk, TaskHandler: string(taskengine.HandleChatCompletion), Content: "Hello from the real client stack."})
		publish(taskengine.TaskEvent{Kind: taskengine.TaskEventToolCallPending, ToolName: "local_fs.read_file", ApprovalID: "call-1", ApprovalArgs: map[string]any{"path": "/tmp/x.txt"}})
		publish(taskengine.TaskEvent{Kind: taskengine.TaskEventToolCall, ToolName: "local_fs.read_file", ApprovalID: "call-1", Content: `"file contents"`})
		publish(taskengine.TaskEvent{Kind: taskengine.TaskEventTokenUsage, TokenUsed: 12, TokenSize: 4096})
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}
	h.swapAgent(newResp.SessionID, fake)

	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hi")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	updates := h.lc.drain(t, 5)
	byKind := make(map[libacp.SessionUpdateKind]libacp.SessionUpdate, len(updates))
	for _, u := range updates {
		byKind[u.Update.SessionUpdate] = u.Update
	}

	require.Contains(t, byKind, libacp.SessionUpdateSessionInfo, "session/prompt always appends a session_info_update")

	chunk, ok := byKind[libacp.SessionUpdateAgentMessageChunk]
	require.True(t, ok)
	require.NotNil(t, chunk.Content)
	require.Equal(t, "Hello from the real client stack.", chunk.Content.Text)

	pending, ok := byKind[libacp.SessionUpdateToolCall]
	require.True(t, ok, "the first notification for a tool call must be create-shaped, not update-shaped")
	require.Equal(t, "call-1", pending.ToolCallID)
	require.Equal(t, libacp.ToolCallStatusPending, pending.Status)

	completed, ok := byKind[libacp.SessionUpdateToolCallUpdate]
	require.True(t, ok)
	require.Equal(t, "call-1", completed.ToolCallID)
	require.Equal(t, libacp.ToolCallStatusCompleted, completed.Status)

	usage, ok := byKind[libacp.SessionUpdateUsageUpdate]
	require.True(t, ok)
	require.Equal(t, 12, usage.Used)
	require.Equal(t, 4096, usage.Size)
}

// TestLoopback_Prompt_PushesDerivedTitleInSessionInfo pins: the post-turn session_info_update carries a title derived from the session's first user message.
func TestLoopback_Prompt_PushesDerivedTitleInSessionInfo(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1) // deferred available_commands_update

	// Persisted against the internal id, distinct from the ACP session id.
	h.tr.sessionMu.Lock()
	internalID := h.tr.sessions[newResp.SessionID].InternalSessionID
	h.tr.sessionMu.Unlock()
	require.NotEmpty(t, internalID)

	const firstMessage = "   hey   how do you do?   "
	require.NoError(t, chatservice.NewManager("loopback-ws").PersistDiff(ctx, h.tr.deps.DB.WithoutTransaction(), internalID, []taskengine.Message{
		{Role: "user", Content: firstMessage, Timestamp: time.Now()},
		{Role: "assistant", Content: "very well, thank you", Timestamp: time.Now().Add(time.Second)},
	}))

	fake := &loopbackAgent{promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}}
	h.swapAgent(newResp.SessionID, fake)

	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hi again")},
	})
	require.NoError(t, err)

	notes := h.lc.drain(t, 1)
	info := notes[0].Update
	require.Equal(t, libacp.SessionUpdateSessionInfo, info.SessionUpdate)
	require.NotEmpty(t, info.UpdatedAt, "the freshness ping must still be present")
	require.Equal(t, "hey how do you do?", info.Title,
		"session_info_update must carry the first user message (whitespace-collapsed) as Title, not the raw session id")
	require.NotEqual(t, string(newResp.SessionID), info.Title,
		"the derived title must not echo the session id, or the client treats it as absent")
}

// TestLoopback_CancelPrompt_ResolvesStopReasonCancelled pins: CancelPrompt mid-flight resolves with stopReason cancelled and no JSON-RPC error.
func TestLoopback_CancelPrompt_ResolvesStopReasonCancelled(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	started := make(chan struct{})
	var startOnce sync.Once
	fake := &loopbackAgent{promptFunc: func(ctx context.Context, _ agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		startOnce.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return &agentservice.PromptResponse{StopReason: agentservice.StopCancelled}, ctx.Err()
		case <-time.After(5 * time.Second):
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		}
	}}
	h.swapAgent(newResp.SessionID, fake)

	type result struct {
		resp libacp.PromptResponse
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
			SessionID: newResp.SessionID,
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("please cancel me")},
		})
		resultCh <- result{resp, err}
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not reach the fake agent")
	}

	require.NoError(t, h.client.CancelPrompt(newResp.SessionID))

	select {
	case r := <-resultCh:
		require.NoError(t, r.err, "ACP spec: cancellation must not surface as a JSON-RPC error")
		require.Equal(t, libacp.StopReasonCancelled, r.resp.StopReason)
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not resolve after CancelPrompt")
	}
}

// TestLoopback_ServerCancel_AbortsEngineCtxAndResolvesCancelled pins: a session/cancel notification aborts the in-flight turn's engine context (not just the wire) and resolves stopReason cancelled.
func TestLoopback_ServerCancel_AbortsEngineCtxAndResolvesCancelled(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	started := make(chan struct{})
	var startOnce sync.Once
	engineCtxCancelled := make(chan struct{})
	fake := &loopbackAgent{promptFunc: func(ctx context.Context, _ agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		startOnce.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			close(engineCtxCancelled)
			return &agentservice.PromptResponse{StopReason: agentservice.StopCancelled}, ctx.Err()
		case <-time.After(5 * time.Second):
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		}
	}}
	h.swapAgent(newResp.SessionID, fake)

	type result struct {
		resp libacp.PromptResponse
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := h.client.Prompt(ctx, libacp.PromptRequest{
			SessionID: newResp.SessionID,
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent("start a slow turn")},
		})
		resultCh <- result{resp, err}
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not reach the fake agent")
	}

	require.NoError(t, h.client.CancelSession(libacp.CancelNotification{SessionID: newResp.SessionID}))

	select {
	case <-engineCtxCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("engine ctx was not cancelled after session/cancel — the running turn kept executing")
	}

	select {
	case r := <-resultCh:
		require.NoError(t, r.err, "ACP spec: cancellation must not surface as a JSON-RPC error")
		require.Equal(t, libacp.StopReasonCancelled, r.resp.StopReason)
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not resolve promptly after session/cancel")
	}
}

// TestUnit_Cancel_NoInflightTurnIsCleanNoOp pins: session/cancel with no running turn is a clean no-op, never an error.
func TestUnit_Cancel_NoInflightTurnIsCleanNoOp(t *testing.T) {
	tr := &Transport{promptCancels: make(map[libacp.SessionID]*inflightPrompt)}
	require.False(t, tr.cancelInflightPrompt("no-such-session"),
		"cancelling a session with no in-flight turn must report it cancelled nothing")
	require.NoError(t, tr.Cancel(context.Background(), libacp.CancelNotification{SessionID: "no-such-session"}),
		"session/cancel with no in-flight turn is a clean no-op, never an error")
}

// TestUnit_PromptCancel_RegisterSupersedeUnregister pins: a second registration supersedes and cancels the first; a stale unregister never evicts a newer turn's registration.
func TestUnit_PromptCancel_RegisterSupersedeUnregister(t *testing.T) {
	tr := &Transport{promptCancels: make(map[libacp.SessionID]*inflightPrompt)}
	const sid = libacp.SessionID("sess-x")

	firstCancelled := false
	reg1 := tr.registerPromptCancel(sid, func() { firstCancelled = true })

	secondCancelled := false
	reg2 := tr.registerPromptCancel(sid, func() { secondCancelled = true })
	require.True(t, firstCancelled, "a superseding registration must cancel the stale turn")

	tr.unregisterPromptCancel(sid, reg1)
	require.True(t, tr.cancelInflightPrompt(sid), "the current (second) turn must still be registered")
	require.True(t, secondCancelled)

	tr.unregisterPromptCancel(sid, reg2)
	require.False(t, tr.cancelInflightPrompt(sid), "after the current turn unregisters, nothing remains")
}

// TestLoopback_Prompt_PermissionRoundTripThroughRealClient pins: session/request_permission round-trips end to end against a real client, for both allow and deny.
func TestLoopback_Prompt_PermissionRoundTripThroughRealClient(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	var approvalErr error
	var allowed bool
	fake := &loopbackAgent{promptFunc: func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		approveCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, req.SessionID)
		allowed, approvalErr = h.tr.AskApproval(approveCtx, hitlservice.ApprovalRequest{
			ToolCallID: "call-perm-1",
			ToolName:   "local_shell.exec",
			Args:       map[string]any{"command": "rm -rf /tmp/x"},
		})
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}}
	h.swapAgent(newResp.SessionID, fake)

	h.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do the thing")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)
	require.NoError(t, approvalErr)
	require.True(t, allowed, "client answered allow_once; AskApproval must resolve true")

	req, ok := h.lc.lastPermissionRequest()
	require.True(t, ok, "the real client must have received session/request_permission")
	require.Equal(t, newResp.SessionID, req.SessionID)
	require.Equal(t, "call-perm-1", req.ToolCall.ToolCallID)

	h.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionDeny},
	})
	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("do it again")},
	})
	require.NoError(t, err)
	require.NoError(t, approvalErr)
	require.False(t, allowed, "client answered reject; AskApproval must resolve false")
}

// TestLoopback_SessionRouter_RoutesToOwningTransport pins the shared SessionRouter, keyed by internal session id: (a) a live session routes to its owning transport, (b) an unknown session yields ErrNoBoundSession, (c) a closed session stops routing.
func TestLoopback_SessionRouter_RoutesToOwningTransport(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	h.tr.sessionMu.Lock()
	internalID := h.tr.sessions[newResp.SessionID].InternalSessionID
	h.tr.sessionMu.Unlock()
	require.NotEmpty(t, internalID)

	h.lc.setPermissionResponse(libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: approvalflow.OptionAllow},
	})
	approveCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, internalID)
	allowed, err := h.router.AskApproval(approveCtx, hitlservice.ApprovalRequest{
		ToolCallID: "router-call-1",
		ToolName:   "local_fs.write_file",
		Args:       map[string]any{"path": "/tmp/loopback-router/x.txt"},
	})
	require.NoError(t, err)
	require.True(t, allowed, "router must bridge to the owning transport and resolve the client's allow")
	req, ok := h.lc.lastPermissionRequest()
	require.True(t, ok, "the real client must have received session/request_permission")
	require.Equal(t, newResp.SessionID, req.SessionID)
	require.Equal(t, "router-call-1", req.ToolCall.ToolCallID)

	_, err = h.router.AskApproval(
		context.WithValue(ctx, runtimetypes.SessionIDContextKey, "no-such-session"),
		hitlservice.ApprovalRequest{ToolCallID: "router-call-2", ToolName: "local_fs.write_file"},
	)
	require.ErrorIs(t, err, ErrNoBoundSession)

	_, err = h.client.CloseSession(ctx, libacp.CloseSessionRequest{SessionID: newResp.SessionID})
	require.NoError(t, err)
	_, err = h.router.AskApproval(approveCtx, hitlservice.ApprovalRequest{ToolCallID: "router-call-3", ToolName: "local_fs.write_file"})
	require.ErrorIs(t, err, ErrNoBoundSession, "a closed session must no longer route")
}

// TestLoopback_Prompt_FSReadWriteThroughRealClient pins: fs/read_text_file and fs/write_text_file round-trip through ACPFileIO against a real client.
func TestLoopback_Prompt_FSReadWriteThroughRealClient(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{
			FS: libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
		},
	})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	h.lc.mu.Lock()
	h.lc.files["/tmp/loopback-fs/note.txt"] = "hello from the client"
	h.lc.mu.Unlock()

	fio := NewACPFileIO(func() *Transport { return h.tr })
	var readBack []byte
	fake := &loopbackAgent{promptFunc: func(ctx context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
		approveCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, req.SessionID)
		var readErr, writeErr error
		readBack, readErr = fio.ReadFile(approveCtx, "/tmp/loopback-fs/note.txt")
		require.NoError(t, readErr)
		writeErr = fio.WriteFile(approveCtx, "/tmp/loopback-fs/written.txt", []byte("hello from the agent"))
		require.NoError(t, writeErr)
		return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
	}}
	h.swapAgent(newResp.SessionID, fake)

	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("read and write")},
	})
	require.NoError(t, err)
	require.Equal(t, "hello from the client", string(readBack))

	h.lc.mu.Lock()
	written := h.lc.files["/tmp/loopback-fs/written.txt"]
	h.lc.mu.Unlock()
	require.Equal(t, "hello from the agent", written)
}

// TestLoopback_SetSessionConfigOption_RoundTripThroughRealClient pins: set_config_option's change (think level) reflects in the response and durably applies to the session.
func TestLoopback_SetSessionConfigOption_RoundTripThroughRealClient(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	thinkOption := func(options []libacp.SessionConfigOption) libacp.SessionConfigOption {
		t.Helper()
		for _, o := range options {
			if o.ID == configIDThink {
				return o
			}
		}
		t.Fatalf("think config option missing from %#v", options)
		return libacp.SessionConfigOption{}
	}
	require.Equal(t, "high", thinkOption(newResp.ConfigOptions).CurrentValue, "session/new's default think level")

	setResp, err := h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  configIDThink,
		Value:     libacp.StringConfigValue("xhigh"),
	})
	require.NoError(t, err)
	require.Equal(t, "xhigh", thinkOption(setResp.ConfigOptions).CurrentValue)

	h.tr.sessionMu.Lock()
	sess := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	require.Equal(t, "xhigh", sess.think(), "the change must durably apply to the session's live state")
}

// TestLoopback_UnknownSlashCommand_AnsweredLocally pins: a mistyped slash command is answered locally by the server (one teaching chunk, end_turn) and never reaches the model.
func TestLoopback_UnknownSlashCommand_AnsweredLocally(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1) // deferred available_commands_update

	var promptCalls int32
	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			atomic.AddInt32(&promptCalls, 1)
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/totallyfakecommand")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)
	require.Zero(t, atomic.LoadInt32(&promptCalls), "an unknown command must not cost a model turn")

	updates := h.lc.drain(t, 1)
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, updates[0].Update.SessionUpdate)
	require.NotNil(t, updates[0].Update.Content)
	require.Contains(t, updates[0].Update.Content.Text, "/totallyfakecommand")
	require.Contains(t, updates[0].Update.Content.Text, "/help")

	select {
	case extra := <-h.lc.updates:
		t.Fatalf("unknown command produced a second update: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}

	h.tr.sessionMu.Lock()
	internalID := h.tr.sessions[newResp.SessionID].InternalSessionID
	h.tr.sessionMu.Unlock()
	msgs, err := chatservice.NewManager("loopback-ws").ListMessages(ctx, h.tr.deps.DB.WithoutTransaction(), internalID)
	require.NoError(t, err)
	require.Empty(t, msgs, "an unknown command is not an act worth recording")
}

// TestLoopback_PastedPathStillReachesTheModel pins: an absolute path pasted as a prompt still reaches the model, never answered as a command.
func TestLoopback_PastedPathStillReachesTheModel(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	seen := make(chan string, 4)
	h.swapAgent(newResp.SessionID, &loopbackAgent{
		promptFunc: func(_ context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
			seen <- req.Input
			return &agentservice.PromptResponse{StopReason: agentservice.StopEndTurn}, nil
		},
	})

	for _, input := range []string{"/etc/passwd", "/home/x y", "what does /etc do"} {
		_, err := h.client.Prompt(ctx, libacp.PromptRequest{
			SessionID: newResp.SessionID,
			Prompt:    []libacp.ContentBlock{libacp.NewTextContent(input)},
		})
		require.NoError(t, err)
		select {
		case got := <-seen:
			require.Equal(t, input, got, "%q must reach the model verbatim", input)
		case <-time.After(5 * time.Second):
			t.Fatalf("%q never reached the model — the command-shape test is too greedy", input)
		}
	}
}

// TestLoopback_LoadSession_ReplaysFailedToolsAndRealUsage pins: a reloaded session with a failed tool call replays that failure, and its usage gauge reflects real history cost.
func TestLoopback_LoadSession_ReplaysFailedToolsAndRealUsage(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	cwd := t.TempDir()
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: cwd, McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	h.tr.sessionMu.Lock()
	internalID := h.tr.sessions[newResp.SessionID].InternalSessionID
	h.tr.sessionMu.Unlock()

	// The assistant opens two calls; the results, written later, are the
	// only record of how each ended.
	now := time.Now().UTC()
	history := []taskengine.Message{
		{ID: "m1", Role: "user", Content: "read both files", Timestamp: now},
		{ID: "m2", Role: "assistant", Content: "on it", Timestamp: now.Add(time.Millisecond), CallTools: []taskengine.ToolCall{
			{ID: "ok-1", Type: "function", Function: taskengine.FunctionCall{Name: "local_fs.read_file", Arguments: `{"path":"/tmp/there.txt"}`}},
			{ID: "bad-1", Type: "function", Function: taskengine.FunctionCall{Name: "local_fs.read_file", Arguments: `{"path":"/tmp/gone.txt"}`}},
		}},
		{ID: "m3", Role: "tool", ToolCallID: "ok-1", Content: `"file contents"`, Timestamp: now.Add(2 * time.Millisecond)},
		{ID: "m4", Role: "tool", ToolCallID: "bad-1", Content: `"tool local_fs.read_file execution failed: open /tmp/gone.txt: no such file or directory"`, Timestamp: now.Add(3 * time.Millisecond)},
	}
	require.NoError(t, chatservice.NewManager("loopback-ws").
		PersistDiff(ctx, h.tr.deps.DB.WithoutTransaction(), internalID, history))

	_, err = h.client.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: newResp.SessionID, Cwd: cwd})
	require.NoError(t, err)

	updates := h.lc.drain(t, 8)

	byToolCall := map[string][]libacp.SessionUpdate{}
	var usage *libacp.SessionUpdate
	for i := range updates {
		u := updates[i].Update
		if u.ToolCallID != "" {
			byToolCall[u.ToolCallID] = append(byToolCall[u.ToolCallID], u)
		}
		if u.SessionUpdate == libacp.SessionUpdateUsageUpdate {
			usage = &updates[i].Update
		}
	}

	require.Len(t, byToolCall["ok-1"], 2, "a replayed call is an opening card plus its closing update")
	for _, u := range byToolCall["ok-1"] {
		require.Equal(t, libacp.ToolCallStatusCompleted, u.Status)
	}
	require.Len(t, byToolCall["bad-1"], 2)
	for _, u := range byToolCall["bad-1"] {
		require.Equal(t, libacp.ToolCallStatusFailed, u.Status,
			"a call the transcript records as failed must not replay as a green check (%s)", u.SessionUpdate)
	}

	require.NotNil(t, usage, "a loaded session must be told what it has already used")
	require.Equal(t, estimateHistoryTokens(history), usage.Used)
	require.Positive(t, usage.Used, "the gauge must not come back claiming a full history cost nothing")
}

// TestUnit_SessionTokenSize_MirrorsTheEnginesOwnBudgetArithmetic pins: the pre-turn gauge denominator matches taskenv's ctxLength resolution (chain token_limit, narrowed by a smaller session override).
func TestUnit_SessionTokenSize_MirrorsTheEnginesOwnBudgetArithmetic(t *testing.T) {
	const sid = libacp.SessionID("s")
	newTransport := func(chainLimit int64, sessionLimit int) *Transport {
		tr := &Transport{
			sessions:        map[libacp.SessionID]*sessionEntry{sid: {EffectiveTokenLimit: sessionLimit}},
			contenoxToACPID: make(map[string]libacp.SessionID),
		}
		tr.deps.ChainRegistry = &ChainRegistry{defaultChain: &taskengine.TaskChainDefinition{TokenLimit: chainLimit}}
		return tr
	}

	require.Equal(t, 131072, newTransport(131072, 0).sessionTokenSize(context.Background(), sid),
		"with no session override the chain's token_limit IS the budget")
	require.Equal(t, 8192, newTransport(131072, 8192).sessionTokenSize(context.Background(), sid),
		"a smaller session override wins, exactly as taskenv narrows ctxLength")
	require.Equal(t, 131072, newTransport(131072, 999999).sessionTokenSize(context.Background(), sid),
		"a larger session override does NOT widen the chain's budget")
	require.Equal(t, 4096, newTransport(0, 4096).sessionTokenSize(context.Background(), sid),
		"a chain with no declared budget leaves the session override standing alone")
	require.Zero(t, newTransport(0, 0).sessionTokenSize(context.Background(), sid),
		"with no budget anywhere and no model context length, there is no honest denominator")
}
