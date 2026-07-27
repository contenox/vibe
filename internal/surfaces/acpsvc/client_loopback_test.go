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

	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This file drives the REAL production acpsvc Agent (Transport) through a
// REAL libacp.ClientSideConnection over an in-memory duplex pipe — both
// Run loops live, exactly as an editor and `contenox acp` would talk to each
// other, except the transport is io.Pipe instead of stdio. It replaces the
// raw-frame wireClient assertions in wire_e2e_test.go with the production
// client stack (client.go/clientconn.go) on the other end: the point is
// proving the two finished halves of libacp interoperate, not re-testing
// either one in isolation (that's what clientconn_test.go and this package's
// existing unit tests already do).
//
// Deps are mocked the same way the rest of this package's tests mock them:
// sessionEntry.Agent is swapped for a scripted agentservice.Agent double
// after a real session/new call (mirroring prompt_test.go's fakeAgent).
// There is no real LLM backend and no real chain execution engine anywhere
// in this file.
//
// The event bus, however, is NOT mocked with libbus.NewInMem() the way the
// rest of the package's tests mock it — it is the SQLite backend serve wires
// (contenoxcli/serve_cmd.go), on this harness's own SQLite database. That is
// load-bearing, not incidental. prompt.go tears a turn's event subscription
// down the instant the agent returns:
//
//	sub.Unsubscribe(); close(rawCh); <-translateDone
//
// which delivers the turn's trailing events only on a backend that hands over
// what was published before Unsubscribe. libbus's conformance suite records
// that as a per-backend divergence — drainsOnUnsubscribe is SQLite-only, and
// it says in as many words that "callers that need the last event must not
// assume the SQLite behaviour". On InMem the queued events race the teardown:
// the delivery goroutine selects between a closed done channel and a
// non-empty queue, so under parallel load (this package is subprocess-heavy)
// it can exit having forwarded none of them. The events are then GONE, not
// late — which is why this presented as a flake no timeout could fix.
//
// So an InMem harness would have been asserting that a turn's events reach the
// client while running on a backend whose contract does not promise it. Using
// the backend production uses makes the assertion mean what it says.

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
	// Generous deadline: some callers wait on a spawned subprocess, and under
	// full-suite load spawn + roundtrip can far exceed an isolated run.
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

// loopbackAgent is an agentservice.Agent double whose Prompt behavior each
// test script directly — streaming events onto the bus, calling back into
// the Transport's client-facing seams (AskApproval, ACPFileIO), or blocking
// on ctx cancellation. Every other method is a no-op: session lifecycle
// itself is exercised through the real agentservice.Agent that NewSession
// already wired up (agentservice.New, DB-backed); only Prompt is swapped
// in afterward, mirroring prompt_test.go's fakeAgent one level up the stack.
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

// loopbackHarness wires the real production Transport (acpsvc's Agent
// implementation, New(Deps)) to a real libacp.ClientSideConnection over an
// in-memory duplex pipe, both Run loops live — the agent-side half of this
// is exactly wire_e2e_test.go's setup; the client-side half is what this
// slice adds.
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

	// Same backend serve wires, on the schema this DB was already created with
	// (runtimetypes.SchemaSQLite owns bus_events/bus_requests/bus_replies). The
	// poll interval is shortened only so a turn's events surface promptly; the
	// delivery GUARANTEE this harness relies on is Unsubscribe's final drain,
	// which does not depend on the tick at all. See the file header.
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{
		EventPoll:   5 * time.Millisecond,
		RequestPoll: 5 * time.Millisecond,
	})
	// serve wires a shared SessionRouter so a single engine can route HITL
	// approvals to the owning WS connection; the harness mirrors that so the
	// router path is exercised exactly as production does. It is inert for tests
	// that never consult it.
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
		// Before the DB: the bus owns a cleanup goroutine that queries it.
		require.NoError(t, bus.Close())
		require.NoError(t, db.Close())
	})

	return &loopbackHarness{t: t, tr: tr, client: clientConn, lc: lc, bus: bus, router: router}
}

// swapAgent installs a into sid's live sessionEntry, replacing the real
// agentservice.Agent that NewSession created for the duration of the test's
// Prompt calls — the same white-box seam prompt_test.go uses one layer down.
// NewSession backs a native session with a nativeDriver, so the swap reaches
// into it.
func (h *loopbackHarness) swapAgent(sid libacp.SessionID, a agentservice.Agent) {
	h.tr.sessionMu.Lock()
	h.tr.sessions[sid].driver.(*nativeDriver).agent = a
	h.tr.sessionMu.Unlock()
}

// TestLoopback_InitializeAdvertisesSpecCapabilities proves the real client
// stack can complete "initialize" against the real Transport and pins the
// capability-honesty contract: session lifecycle capabilities the Transport
// actually implements are advertised, and additionalDirectories — which
// NewSession/LoadSession/ResumeSession never read — is not.
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

// TestLoopback_Prompt_StreamsUpdatesThroughRealClient drives initialize ->
// session/new -> session/prompt end to end and proves a streamed turn — an
// assistant chunk, a tool call's pending/completed pair, and a token usage
// update — arrives at the real Client.SessionUpdate handler, alongside the
// session_info_update session/prompt always appends.
//
// Note on ordering: session_info_update is NOT last on the wire, even though
// prompt.go schedules it via libacp.AfterResponse "so it runs after the
// turn". For session/prompt specifically, the cancelable per-turn context
// conn.go's callMethod substitutes (promptCtx = pc.ctx, registered by
// registerPromptCancel before handleRequest ever attaches its
// after-response sink) does not carry that sink, so AfterResponse falls
// back to its synchronous "no sink in ctx" branch (conn.go's AfterResponse
// doc comment) and runs immediately — before the turn's own streamed
// events, which are flushed later, when Prompt's deferred bus-drain runs.
// That is existing, already-shipped behavior this test simply documents
// rather than assumes away; it is orthogonal to this slice's scope (libacp
// connection internals are out of bounds here) and asserted by kind, not
// position, below.
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

// TestLoopback_Prompt_PushesDerivedTitleInSessionInfo pins the beam/ACP
// regression fix: the post-turn session_info_update must carry a Title derived
// from the session's first user message. A session created THIS connection
// received no title in its session/new SessionInfo, so without the pushed title
// the client's tab/sidebar label is stuck on the raw-id fallback ("Sitzung
// acp-XXXX") until a full session/list re-list (only on reconnect). The push
// reuses session/list's first-user-message heuristic, so the live label and a
// later re-list agree.
func TestLoopback_Prompt_PushesDerivedTitleInSessionInfo(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1) // deferred available_commands_update

	// Persist the session's first user message against its internal id (mi.id),
	// which is distinct from the ACP-level session id (mi.name) session/new
	// returned — exactly what a real turn's persistHistory would have stored.
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

// TestLoopback_CancelPrompt_ResolvesStopReasonCancelled cancels a prompt
// turn mid-flight through the real client's CancelPrompt and proves the
// production agent side resolves it with stopReason "cancelled" and no
// JSON-RPC error, per the spec's cancellation contract.
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
			// Mirrors the real agentservice.Agent's own cancellation behavior
			// (see TestUnit_Prompt_CancelledStopReasonReturnsNilError).
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

// TestLoopback_ServerCancel_AbortsEngineCtxAndResolvesCancelled drives the
// SERVER-owned cancellation path added to Transport.Cancel: a session/cancel
// notification (what beam's Stopp button and any editor send) must abort the
// in-flight turn's engine context and resolve the prompt with stopReason
// "cancelled". Unlike the CancelPrompt test above — which also exercises
// libacp's connection-level promptCtx substitution — this asserts the engine's
// own ctx was cancelled (proving cancellation reached the chain/provider layer,
// not just the wire) and drives the raw session/cancel notification directly.
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
			// The engine's own context observed the cancellation — the whole point:
			// cancellation reached the chain-execution layer, not just the wire.
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

	// Send the raw session/cancel notification, exactly as beam/an editor does,
	// so the server's Transport.Cancel owns the abort.
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

// TestUnit_Cancel_NoInflightTurnIsCleanNoOp pins the invariant that a
// session/cancel with no running turn (a client cancelling after the turn
// already finished, or before any prompt) is a clean no-op: Cancel returns nil
// and cancelInflightPrompt reports it cancelled nothing.
func TestUnit_Cancel_NoInflightTurnIsCleanNoOp(t *testing.T) {
	tr := &Transport{promptCancels: make(map[libacp.SessionID]*inflightPrompt)}
	require.False(t, tr.cancelInflightPrompt("no-such-session"),
		"cancelling a session with no in-flight turn must report it cancelled nothing")
	require.NoError(t, tr.Cancel(context.Background(), libacp.CancelNotification{SessionID: "no-such-session"}),
		"session/cancel with no in-flight turn is a clean no-op, never an error")
}

// TestUnit_PromptCancel_RegisterSupersedeUnregister pins the per-session
// registry's lifecycle: a second registration supersedes and cancels the first
// (one turn per session; nothing outlives its turn), and a stale unregister
// never removes a newer turn's registration.
func TestUnit_PromptCancel_RegisterSupersedeUnregister(t *testing.T) {
	tr := &Transport{promptCancels: make(map[libacp.SessionID]*inflightPrompt)}
	const sid = libacp.SessionID("sess-x")

	firstCancelled := false
	reg1 := tr.registerPromptCancel(sid, func() { firstCancelled = true })

	secondCancelled := false
	reg2 := tr.registerPromptCancel(sid, func() { secondCancelled = true })
	require.True(t, firstCancelled, "a superseding registration must cancel the stale turn")

	// The first turn's deferred unregister must not evict the second turn.
	tr.unregisterPromptCancel(sid, reg1)
	require.True(t, tr.cancelInflightPrompt(sid), "the current (second) turn must still be registered")
	require.True(t, secondCancelled)

	tr.unregisterPromptCancel(sid, reg2)
	require.False(t, tr.cancelInflightPrompt(sid), "after the current turn unregisters, nothing remains")
}

// TestLoopback_Prompt_PermissionRoundTripThroughRealClient exercises the
// permission client-callback flow reachable with mocked deps: the fake
// agent calls Transport.AskApproval directly — standing in for the real
// engine's HITL wrapper (localtools.NewHITLWrapper, wired in
// runtime/enginesvc/engine.go, which calls exactly this method when a gated
// tool call is hit mid-chain execution) — to prove the session/
// request_permission round trip works end to end against a real
// ClientSideConnection, for both the allow and the deny outcome.
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

	// The client answers "allow".
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

	// The client rejects the next request.
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

// TestLoopback_SessionRouter_RoutesToOwningTransport pins the serve HITL
// bridge: serve runs many ACP WS connections behind ONE engine, so its single
// AskApproval callback dispatches through a shared SessionRouter keyed by the
// contenox session id in ctx (exactly what the engine's HITL wrapper carries).
// This proves (a) a gated tool call for a live session routes to the owning
// transport's session/request_permission — reaching the real client — and
// resolves the client's outcome; (b) an unknown session yields
// ErrNoBoundSession so serve falls back to its approval-API path; and (c) after
// the session is closed the router no longer routes it. Without the fix, serve
// wired AskApproval straight to the approval-API path, so a beam gated tool call
// hung forever as "Ausstehend" with no permission prompt.
func TestLoopback_SessionRouter_RoutesToOwningTransport(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	h.lc.drain(t, 1)

	// The engine's HITL wrapper carries the INTERNAL contenox session id in ctx
	// (agentservice stamps SessionIDContextKey from it) — the router's key.
	h.tr.sessionMu.Lock()
	internalID := h.tr.sessions[newResp.SessionID].InternalSessionID
	h.tr.sessionMu.Unlock()
	require.NotEmpty(t, internalID)

	// (a) A live session routes to the owning transport; the client answers allow.
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

	// (b) An unknown session is not routable: the caller must fall back.
	_, err = h.router.AskApproval(
		context.WithValue(ctx, runtimetypes.SessionIDContextKey, "no-such-session"),
		hitlservice.ApprovalRequest{ToolCallID: "router-call-2", ToolName: "local_fs.write_file"},
	)
	require.ErrorIs(t, err, ErrNoBoundSession)

	// (c) Closing the session deregisters it from the router.
	_, err = h.client.CloseSession(ctx, libacp.CloseSessionRequest{SessionID: newResp.SessionID})
	require.NoError(t, err)
	_, err = h.router.AskApproval(approveCtx, hitlservice.ApprovalRequest{ToolCallID: "router-call-3", ToolName: "local_fs.write_file"})
	require.ErrorIs(t, err, ErrNoBoundSession, "a closed session must no longer route")
}

// TestLoopback_Prompt_FSReadWriteThroughRealClient exercises the other
// mocked-deps-reachable client-callback flow: fs/read_text_file and
// fs/write_text_file through acpsvc's ACPFileIO (fileio.go), which routes
// through Transport.conn exactly like AskApproval routes through it for
// permissions.
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

// TestLoopback_SetSessionConfigOption_RoundTripThroughRealClient drives
// session/set_config_option through the real client and proves the change
// (here, the "think" level) is both reflected in the response and durably
// applied to the session's live state.
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

// TestLoopback_UnknownSlashCommand_AnsweredLocally is M6 over the real wire: a
// mistyped slash command must be answered by the SERVER — one teaching chunk,
// end_turn — and must never reach the model. Before this, "/totallyfakecommand"
// fell through as ordinary prompt text and bought a real turn whose only output
// was an improvised, differently-worded error every time.
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

	// Exactly one update: no usage_update (nothing was spent, so the gauge must
	// not move) and no session_info_update (nothing happened to the session).
	select {
	case extra := <-h.lc.updates:
		t.Fatalf("unknown command produced a second update: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}

	// And nothing was written to the durable transcript: a typo must not become
	// the session's first user message, and therefore its title, forever.
	h.tr.sessionMu.Lock()
	internalID := h.tr.sessions[newResp.SessionID].InternalSessionID
	h.tr.sessionMu.Unlock()
	msgs, err := chatservice.NewManager("loopback-ws").ListMessages(ctx, h.tr.deps.DB.WithoutTransaction(), internalID)
	require.NoError(t, err)
	require.Empty(t, msgs, "an unknown command is not an act worth recording")
}

// TestLoopback_PastedPathStillReachesTheModel is the other side of the same
// decision, and the one a regression would be silent about: an absolute path
// pasted as a prompt still buys a real turn. If this ever fails, the shape test
// in unknownCommandName has grown teeth it must not have.
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

// TestLoopback_LoadSession_ReplaysFailedToolsAndRealUsage is M12 and M11 in one
// reload: a session whose history contains a failed tool call must come back
// showing that failure, and its gauge must come back carrying what the history
// actually costs instead of a flat zero.
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

	// A transcript exactly as the engine persists one: the assistant opens two
	// calls, and the results — written LATER — are the only record of how each
	// ended. The failure text is verbatim taskexec's substitution.
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

	// user + assistant text + 2 tool_call + 2 tool_call_update + usage_update,
	// then the deferred available_commands_update.
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

// TestUnit_SessionTokenSize_MirrorsTheEnginesOwnBudgetArithmetic pins the
// denominator under the gauge to the number the very next turn will report.
// taskenv resolves a turn's ctxLength as the chain's token_limit, with a
// requested (per-session) budget winning only when it is SMALLER; a
// pre-turn gauge that disagreed would visibly jump the first time the model
// spoke, for no reason an operator can see.
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
