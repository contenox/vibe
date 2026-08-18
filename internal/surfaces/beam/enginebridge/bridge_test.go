package enginebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/surfaces/beam/dialect"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

const scriptCwd = "/tmp/beam-bridge-test"

const defaultScriptSession = libacp.SessionID("beam-script-1")

type duplexPipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *duplexPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *duplexPipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *duplexPipe) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

type scriptAgent struct {
	libacp.UnimplementedAgent

	conn *libacp.AgentSideConnection

	mu       sync.Mutex
	initReq  libacp.InitializeRequest
	prompts  []libacp.PromptRequest
	cancels  []libacp.SessionID
	lists    []libacp.ListSessionsRequest
	closes   []libacp.SessionID
	deletes  []libacp.SessionID
	extCalls []string

	onNewSession    func(libacp.NewSessionRequest) (libacp.NewSessionResponse, error)
	onLoadSession   func(libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error)
	onResumeSession func(libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error)
	onListSessions  func(libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error)
	onPrompt        func(context.Context, libacp.PromptRequest) (libacp.PromptResponse, error)
	onExt           func(context.Context, string, json.RawMessage) (json.RawMessage, *libacp.Error)
}

func (a *scriptAgent) setNewSession(fn func(libacp.NewSessionRequest) (libacp.NewSessionResponse, error)) {
	a.mu.Lock()
	a.onNewSession = fn
	a.mu.Unlock()
}

func (a *scriptAgent) setLoadSession(fn func(libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error)) {
	a.mu.Lock()
	a.onLoadSession = fn
	a.mu.Unlock()
}

func (a *scriptAgent) setResumeSession(fn func(libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error)) {
	a.mu.Lock()
	a.onResumeSession = fn
	a.mu.Unlock()
}

func (a *scriptAgent) setListSessions(fn func(libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error)) {
	a.mu.Lock()
	a.onListSessions = fn
	a.mu.Unlock()
}

func (a *scriptAgent) setPrompt(fn func(context.Context, libacp.PromptRequest) (libacp.PromptResponse, error)) {
	a.mu.Lock()
	a.onPrompt = fn
	a.mu.Unlock()
}

func (a *scriptAgent) setExt(fn func(context.Context, string, json.RawMessage) (json.RawMessage, *libacp.Error)) {
	a.mu.Lock()
	a.onExt = fn
	a.mu.Unlock()
}

func (a *scriptAgent) initialize() libacp.InitializeRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.initReq
}

func (a *scriptAgent) cancelled() []libacp.SessionID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]libacp.SessionID(nil), a.cancels...)
}

func (a *scriptAgent) waitForCancel(t *testing.T, sid libacp.SessionID) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		for _, got := range a.cancelled() {
			if got == sid {
				return true
			}
		}
		return false
	}, 15*time.Second, 5*time.Millisecond, "the far side never saw session/cancel for %s", sid)
}

func (a *scriptAgent) listed() []libacp.ListSessionsRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]libacp.ListSessionsRequest(nil), a.lists...)
}

func (a *scriptAgent) closedSessions() []libacp.SessionID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]libacp.SessionID(nil), a.closes...)
}

func (a *scriptAgent) deletedSessions() []libacp.SessionID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]libacp.SessionID(nil), a.deletes...)
}

func (a *scriptAgent) Initialize(_ context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	a.mu.Lock()
	a.initReq = req
	a.mu.Unlock()
	return libacp.InitializeResponse{
		ProtocolVersion: libacp.ProtocolVersion,
		AgentInfo:       &libacp.Implementation{Name: "script-agent"},
	}, nil
}

func (a *scriptAgent) NewSession(_ context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	a.mu.Lock()
	fn := a.onNewSession
	a.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return libacp.NewSessionResponse{SessionID: defaultScriptSession}, nil
}

func (a *scriptAgent) LoadSession(_ context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	a.mu.Lock()
	fn := a.onLoadSession
	a.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return libacp.LoadSessionResponse{}, nil
}

func (a *scriptAgent) ResumeSession(_ context.Context, req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error) {
	a.mu.Lock()
	fn := a.onResumeSession
	a.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return libacp.ResumeSessionResponse{}, nil
}

func (a *scriptAgent) ListSessions(_ context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	a.mu.Lock()
	a.lists = append(a.lists, req)
	fn := a.onListSessions
	a.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return libacp.ListSessionsResponse{}, nil
}

func (a *scriptAgent) CloseSession(_ context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error) {
	a.mu.Lock()
	a.closes = append(a.closes, req.SessionID)
	a.mu.Unlock()
	return libacp.CloseSessionResponse{}, nil
}

func (a *scriptAgent) DeleteSession(_ context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error) {
	a.mu.Lock()
	a.deletes = append(a.deletes, req.SessionID)
	a.mu.Unlock()
	return libacp.DeleteSessionResponse{}, nil
}

func (a *scriptAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, req)
	fn := a.onPrompt
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func (a *scriptAgent) Cancel(_ context.Context, req libacp.CancelNotification) error {
	a.mu.Lock()
	a.cancels = append(a.cancels, req.SessionID)
	a.mu.Unlock()
	return nil
}

func (a *scriptAgent) handleExt(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	a.mu.Lock()
	a.extCalls = append(a.extCalls, method)
	fn := a.onExt
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, method, params)
	}
	return nil, libacp.MethodNotFound(method)
}

type harness struct {
	t         *testing.T
	bridge    *Bridge
	agent     *scriptAgent
	agentSide *duplexPipe
	inbox     chan []byte
	cancel    context.CancelFunc
	agentDone chan error
}

func newHarness(t *testing.T, tune ...func(*Deps)) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &duplexPipe{r: agentR, w: agentW}
	clientSide := &duplexPipe{r: clientR, w: clientW}

	agent := &scriptAgent{}
	agentConn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		agent.conn = c
		c.SetExtRequestHandler(agent.handleExt)
		return agent
	})
	agentDone := make(chan error, 1)
	go func() { agentDone <- agentConn.Run(ctx) }()

	inbox := make(chan []byte, 8)
	deps := Deps{
		Conn:       clientSide,
		ClientInfo: &libacp.Implementation{Name: "beam", Version: "test"},
		Inbox:      inbox,
	}
	for _, mutate := range tune {
		mutate(&deps)
	}

	b, err := New(ctx, deps)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, b.Close())
		_ = agentSide.Close()
		cancel()
		select {
		case <-agentDone:
		case <-time.After(15 * time.Second):
			t.Error("the agent side of the loopback did not shut down")
		}
	})

	return &harness{
		t:         t,
		bridge:    b,
		agent:     agent,
		agentSide: agentSide,
		inbox:     inbox,
		cancel:    cancel,
		agentDone: agentDone,
	}
}

func (h *harness) initSession(ctx context.Context) libacp.SessionID {
	h.t.Helper()
	resp, err := h.bridge.Initialize(ctx)
	require.NoError(h.t, err)
	require.Equal(h.t, libacp.ProtocolVersion, resp.ProtocolVersion)

	newResp, err := h.bridge.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        scriptCwd,
		McpServers: []libacp.McpServer{},
	})
	require.NoError(h.t, err)
	require.NotEmpty(h.t, newResp.SessionID)
	h.bridge.SetActiveSession(newResp.SessionID)
	return newResp.SessionID
}

func (h *harness) notify(sid libacp.SessionID, update libacp.SessionUpdate) {
	h.t.Helper()
	require.NoError(h.t, h.agent.conn.SessionUpdate(libacp.SessionNotification{SessionID: sid, Update: update}))
}

func (h *harness) collect(timeout time.Duration, stop func(Event) bool) []Event {
	h.t.Helper()
	var got []Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-h.bridge.Events():
			if !ok {
				h.t.Fatalf("event channel closed after %d events: %+v", len(got), got)
			}
			got = append(got, ev)
			if stop(ev) {
				return got
			}
		case <-deadline:
			h.t.Fatalf("timed out after %s; saw %d events: %+v", timeout, len(got), got)
		}
	}
}

func firstOfType[T Event](events []Event) (T, bool) {
	for _, ev := range events {
		if typed, ok := ev.(T); ok {
			return typed, true
		}
	}
	var zero T
	return zero, false
}

func requireType[T Event](t *testing.T, ev Event) T {
	t.Helper()
	typed, ok := ev.(T)
	require.Truef(t, ok, "expected %T, got %T (%+v)", *new(T), ev, ev)
	return typed
}

func mustMeta(v map[string]any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func thinkOptions() []libacp.SessionConfigOption {
	return []libacp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Type:         "select",
			CurrentValue: "openai/gpt-5",
			Options: libacp.NewGroupedSessionConfigValues([]libacp.SessionConfigGroup{
				{Group: "openai", Name: "OpenAI", Options: []libacp.SessionConfigValue{
					{Value: "openai/gpt-5", Name: "gpt-5"},
				}},
			}),
		},
		{
			ID:           "think",
			Name:         "Think",
			Type:         "select",
			CurrentValue: "high",
			Options: libacp.NewSessionConfigValues([]libacp.SessionConfigValue{
				{Value: "low", Name: "low"},
				{Value: "high", Name: "high"},
			}),
		},
	}
}

func TestUnit_Deps_ValidateRequiresAConnection(t *testing.T) {
	require.ErrorContains(t, Deps{}.validate(), "Deps.Conn")
	require.NoError(t, Deps{Conn: &duplexPipe{}}.validate(),
		"a connection is the only required dependency")

	_, err := New(context.Background(), Deps{})
	require.ErrorContains(t, err, "Deps.Conn")
}

func TestUnit_Bridge_InitializeDeclaresNoClientSideIO(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	resp, err := h.bridge.Initialize(ctx)
	require.NoError(t, err)
	require.Equal(t, libacp.ProtocolVersion, resp.ProtocolVersion)

	got := h.agent.initialize()
	require.False(t, got.ClientCapabilities.FS.ReadTextFile)
	require.False(t, got.ClientCapabilities.FS.WriteTextFile)
	require.False(t, got.ClientCapabilities.Terminal)
	require.NotNil(t, got.ClientInfo)
	require.Equal(t, "beam", got.ClientInfo.Name)

	c := &bridgeClient{b: h.bridge}
	_, err = c.ReadTextFile(ctx, libacp.ReadTextFileRequest{Path: "/tmp/x"})
	require.Error(t, err)
	_, err = c.WriteTextFile(ctx, libacp.WriteTextFileRequest{Path: "/tmp/x"})
	require.Error(t, err)
	_, err = c.CreateTerminal(ctx, libacp.CreateTerminalRequest{})
	require.Error(t, err)
}

func TestUnit_Bridge_InitializeNamesBeamWithoutClientInfo(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.ClientInfo = nil })

	_, err := h.bridge.Initialize(context.Background())
	require.NoError(t, err)

	got := h.agent.initialize()
	require.NotNil(t, got.ClientInfo)
	require.Equal(t, "beam", got.ClientInfo.Name)
}

func TestUnit_Bridge_NewSessionReplaysItsOpeningConfigOptions(t *testing.T) {
	h := newHarness(t)
	h.agent.setNewSession(func(libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
		return libacp.NewSessionResponse{SessionID: defaultScriptSession, ConfigOptions: thinkOptions()}, nil
	})

	sid := h.initSession(context.Background())

	events := h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(ConfigOptionUpdated)
		return ok
	})
	opts, ok := firstOfType[ConfigOptionUpdated](events)
	require.True(t, ok, "session/new's config options never reached the event stream")
	require.Equal(t, sid, opts.SessionID)

	ids := make([]string, 0, len(opts.Options))
	for _, o := range opts.Options {
		ids = append(ids, o.ID)
	}
	require.Subset(t, ids, []string{"model", "think"})
	require.Equal(t, []string{"low", "high"}, ValueDomains(opts.Options)[dialect.CommandThink])

	provider, model, known := SelectedModel(opts.Options)
	require.True(t, known)
	require.Equal(t, "openai", provider)
	require.Equal(t, "gpt-5", model)
}

func TestUnit_Bridge_NewSessionWithoutConfigOptionsReplaysNothing(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	h.notify(sid, libacp.NewAgentMessageChunk("marker"))
	events := h.collect(10*time.Second, func(ev Event) bool {
		td, ok := ev.(TextDelta)
		return ok && td.Text == "marker"
	})
	_, sawOptions := firstOfType[ConfigOptionUpdated](events)
	require.False(t, sawOptions, "an empty option list must not be replayed as an update")
}

func TestUnit_Bridge_LoadSessionReplaysOptionsThenEndsTheReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const sid = libacp.SessionID("beam-loaded")

	h.agent.setLoadSession(func(req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
		require.Equal(t, sid, req.SessionID)
		require.NoError(t, h.agent.conn.SessionUpdate(libacp.SessionNotification{
			SessionID: sid,
			Update:    libacp.NewAgentMessageChunk("replayed"),
		}))
		return libacp.LoadSessionResponse{ConfigOptions: thinkOptions()}, nil
	})

	h.bridge.SetActiveSession(sid)
	_, err := h.bridge.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: sid, Cwd: scriptCwd})
	require.NoError(t, err)

	events := h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(ReplayEnded)
		return ok
	})

	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, reflect.TypeOf(ev).Name())
	}
	require.Equal(t, []string{"TextDelta", "ConfigOptionUpdated", "ReplayEnded"}, kinds,
		"ReplayEnded must follow every replayed notification and the option replay")

	ended, ok := firstOfType[ReplayEnded](events)
	require.True(t, ok)
	require.Equal(t, sid, ended.SessionID)
}

func TestUnit_Bridge_ResumeSessionReplaysItsOpeningConfigOptions(t *testing.T) {
	h := newHarness(t)
	const sid = libacp.SessionID("beam-resumed")

	h.agent.setResumeSession(func(req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error) {
		require.Equal(t, sid, req.SessionID)
		return libacp.ResumeSessionResponse{ConfigOptions: thinkOptions()}, nil
	})

	_, err := h.bridge.ResumeSession(context.Background(), libacp.ResumeSessionRequest{SessionID: sid, Cwd: scriptCwd})
	require.NoError(t, err)

	events := h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(ConfigOptionUpdated)
		return ok
	})
	opts, ok := firstOfType[ConfigOptionUpdated](events)
	require.True(t, ok)
	require.Equal(t, sid, opts.SessionID)
}

func TestUnit_Bridge_ListSessionsForwardsTheRequestAndTheRoster(t *testing.T) {
	h := newHarness(t)
	roster := []libacp.SessionInfo{
		{SessionID: "beam-newest", Cwd: scriptCwd, Title: "newest"},
		{SessionID: "beam-older", Cwd: scriptCwd, Title: "older"},
	}
	h.agent.setListSessions(func(libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
		return libacp.ListSessionsResponse{Sessions: roster, NextCursor: "c2"}, nil
	})

	resp, err := h.bridge.ListSessions(context.Background(), libacp.ListSessionsRequest{Cwd: scriptCwd, Cursor: "c1"})
	require.NoError(t, err)
	require.Equal(t, roster, resp.Sessions)
	require.Equal(t, "c2", resp.NextCursor)

	listed := h.agent.listed()
	require.Len(t, listed, 1)
	require.Equal(t, scriptCwd, listed[0].Cwd)
	require.Equal(t, "c1", listed[0].Cursor)
}

func TestUnit_Bridge_SetActiveSessionTracksTheLiveSession(t *testing.T) {
	h := newHarness(t)
	require.Empty(t, h.bridge.ActiveSession(), "a fresh bridge watches every session")

	h.bridge.SetActiveSession("beam-a")
	require.Equal(t, libacp.SessionID("beam-a"), h.bridge.ActiveSession())

	h.bridge.SetActiveSession("")
	require.Empty(t, h.bridge.ActiveSession())
}

func TestUnit_Bridge_PromptRunsAFullTurn(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	h.agent.setPrompt(func(_ context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
		require.Len(t, req.Prompt, 1)
		require.Equal(t, "explain", req.Prompt[0].Text)
		for _, text := range []string{"one ", "two ", "three"} {
			if err := h.agent.conn.SessionUpdate(libacp.SessionNotification{
				SessionID: sid,
				Update:    libacp.NewAgentMessageChunk(text),
			}); err != nil {
				return libacp.PromptResponse{}, err
			}
		}
		return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
	})

	require.NoError(t, h.bridge.SubmitPrompt(sid, "explain"))

	events := h.collect(15*time.Second, func(ev Event) bool {
		_, ok := ev.(TurnEnded)
		return ok
	})

	ended, ok := firstOfType[TurnEnded](events)
	require.True(t, ok)
	require.Equal(t, sid, ended.SessionID)
	require.Equal(t, libacp.StopReasonEndTurn, ended.StopReason)

	var streamed string
	for _, ev := range events {
		if td, isText := ev.(TextDelta); isText {
			require.Equal(t, sid, td.SessionID)
			streamed += td.Text
		}
		_, failed := ev.(TurnFailed)
		require.False(t, failed, "a completed turn must not also fail: %+v", ev)
	}
	require.Equal(t, "one two three", streamed)
	require.False(t, h.bridge.hasInflight(sid))
}

func TestUnit_Bridge_CancelledTurnEndsAndNeverFails(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	entered := make(chan struct{})
	h.agent.setPrompt(func(pctx context.Context, _ libacp.PromptRequest) (libacp.PromptResponse, error) {
		close(entered)
		<-pctx.Done()
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	})

	require.NoError(t, h.bridge.SubmitPrompt(sid, "long one"))
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent never received the prompt")
	}
	require.NoError(t, h.bridge.Cancel(sid))

	events := h.collect(15*time.Second, func(ev Event) bool {
		switch ev.(type) {
		case TurnEnded, TurnFailed:
			return true
		}
		return false
	})

	failed, sawFailure := firstOfType[TurnFailed](events)
	require.False(t, sawFailure, "a cancelled turn must not surface as a failure: %+v", failed)

	ended, ok := firstOfType[TurnEnded](events)
	require.True(t, ok)
	require.Equal(t, sid, ended.SessionID)
	require.Equal(t, libacp.StopReasonCancelled, ended.StopReason)
	h.agent.waitForCancel(t, sid)
	require.False(t, h.bridge.hasInflight(sid), "the turn's session must be free again")
}

func TestUnit_Bridge_FailedTurnDoesNotWedgeTheSession(t *testing.T) {
	h := newHarness(t)
	_ = h.initSession(context.Background())

	const ghost = libacp.SessionID("beam-no-such-session")
	h.agent.setPrompt(func(_ context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
		return libacp.PromptResponse{}, libacp.InternalError("unknown session " + string(req.SessionID))
	})

	require.NoError(t, h.bridge.SubmitPrompt(ghost, "hello"))
	events := h.collect(15*time.Second, func(ev Event) bool {
		_, ok := ev.(TurnFailed)
		return ok
	})
	failed, ok := firstOfType[TurnFailed](events)
	require.True(t, ok)
	require.Equal(t, ghost, failed.SessionID)
	require.ErrorContains(t, failed.Err, "unknown session")

	require.False(t, h.bridge.hasInflight(ghost), "a failed turn must release its session")

	require.NoError(t, h.bridge.SubmitPrompt(ghost, "again"),
		"a session whose turn failed must accept the next prompt")
	h.collect(15*time.Second, func(ev Event) bool {
		_, isFailure := ev.(TurnFailed)
		return isFailure
	})
}

func TestUnit_Bridge_SecondPromptForOneSessionIsRejected(t *testing.T) {
	h := newHarness(t)
	const sid = libacp.SessionID("beam-busy")

	h.bridge.promptMu.Lock()
	h.bridge.inflight[sid] = struct{}{}
	h.bridge.promptMu.Unlock()

	require.ErrorIs(t, h.bridge.SubmitPrompt(sid, "second"), ErrPromptInFlight)
	require.False(t, h.bridge.hasInflight("beam-other"), "the gate is per session, not global")

	h.bridge.promptMu.Lock()
	delete(h.bridge.inflight, sid)
	h.bridge.promptMu.Unlock()
}

func TestUnit_Bridge_SubmitPromptRejectsUnusableInput(t *testing.T) {
	h := newHarness(t)
	require.ErrorIs(t, h.bridge.SubmitPrompt("beam-x", ""), ErrEmptyPrompt)
	require.ErrorContains(t, h.bridge.SubmitPrompt("", "hi"), "session id is required")
}

func TestUnit_Bridge_CancelWithoutInflightPromptIsNotAnError(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())
	require.NoError(t, h.bridge.Cancel(sid))
	require.NoError(t, h.bridge.Cancel("beam-never-existed"))
}

func permissionRequest(sid libacp.SessionID, toolCallID string) libacp.RequestPermissionRequest {
	return libacp.RequestPermissionRequest{
		SessionID: sid,
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: toolCallID,
			Title:      "local_fs.write_file",
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusPending,
			RawInput:   json.RawMessage(`{"path":"/tmp/x"}`),
		},
		Options: []libacp.PermissionOption{
			{OptionID: dialect.OptionAllow, Name: "Allow", Kind: libacp.PermissionAllowOnce},
			{OptionID: dialect.OptionDeny, Name: "Deny", Kind: libacp.PermissionRejectOnce},
		},
	}
}

func TestUnit_Bridge_PermissionResolvePlumbing(t *testing.T) {
	tests := []struct {
		name     string
		allow    bool
		wantID   string
		wantKind libacp.PermissionOutcomeKind
	}{
		{"allow", true, dialect.OptionAllow, libacp.PermissionOutcomeSelected},
		{"deny", false, dialect.OptionDeny, libacp.PermissionOutcomeSelected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			meta := dialect.Meta{
				ToolsName:  "local_fs",
				ToolName:   "write_file",
				PolicyName: "cautious",
				PolicyPath: "/policies/cautious.json",
				Diff:       "@@ -1 +1 @@",
				DiffOld:    "old",
				DiffNew:    "new",
			}
			raw, err := json.Marshal(meta)
			require.NoError(t, err)

			req := permissionRequest("beam-perm", "call-7")
			req.ToolCall.Meta = raw

			respCh := make(chan libacp.RequestPermissionResponse, 1)
			go func() {
				resp, err := h.bridge.client.RequestPermission(context.Background(), req)
				if err != nil {
					t.Errorf("RequestPermission: %v", err)
					return
				}
				respCh <- resp
			}()

			events := h.collect(10*time.Second, func(ev Event) bool {
				_, ok := ev.(PermissionRequested)
				return ok
			})
			gate, ok := firstOfType[PermissionRequested](events)
			require.True(t, ok)
			require.Equal(t, libacp.SessionID("beam-perm"), gate.SessionID)
			require.Equal(t, "call-7", gate.ToolCallID)
			require.Equal(t, libacp.ToolKindEdit, gate.Kind)
			require.Equal(t, libacp.ToolCallStatusPending, gate.Status)
			require.Equal(t, meta, gate.Meta, "the _meta envelope must reach the card decoded")
			require.JSONEq(t, `{"path":"/tmp/x"}`, string(gate.RawInput))
			require.Len(t, gate.Options, 2)
			require.NotNil(t, gate.Resolve)

			gate.Resolve(tt.allow)
			gate.Resolve(!tt.allow)

			select {
			case resp := <-respCh:
				require.Equal(t, tt.wantKind, resp.Outcome.Outcome)
				require.Equal(t, tt.wantID, resp.Outcome.OptionID)
			case <-time.After(10 * time.Second):
				t.Fatal("RequestPermission did not return after Resolve")
			}
		})
	}
}

func TestUnit_Bridge_PermissionMetaFallsBackToTheRequestEnvelope(t *testing.T) {
	h := newHarness(t)
	meta := dialect.Meta{ToolsName: "local_fs", ToolName: "write_file", PolicyName: "cautious"}
	raw, err := json.Marshal(meta)
	require.NoError(t, err)

	req := permissionRequest("beam-perm", "call-8")
	req.Meta = raw

	go func() {
		if _, err := h.bridge.client.RequestPermission(context.Background(), req); err != nil {
			t.Errorf("RequestPermission: %v", err)
		}
	}()

	events := h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(PermissionRequested)
		return ok
	})
	gate, ok := firstOfType[PermissionRequested](events)
	require.True(t, ok)
	require.Equal(t, meta, gate.Meta)
	gate.Resolve(false)
}

func TestUnit_Bridge_PendingPermissionResolvesCancelledOnClose(t *testing.T) {
	h := newHarness(t)

	respCh := make(chan libacp.RequestPermissionResponse, 1)
	go func() {
		resp, err := h.bridge.client.RequestPermission(context.Background(), permissionRequest("beam-perm", "call-1"))
		if err != nil {
			t.Errorf("RequestPermission: %v", err)
			return
		}
		respCh <- resp
	}()

	h.collect(10*time.Second, func(ev Event) bool {
		_, ok := ev.(PermissionRequested)
		return ok
	})

	require.NoError(t, h.bridge.Close())

	select {
	case resp := <-respCh:
		require.Equal(t, libacp.PermissionOutcomeCancelled, resp.Outcome.Outcome)
		require.Empty(t, resp.Outcome.OptionID)
	case <-time.After(10 * time.Second):
		t.Fatal("a pending permission request survived Close")
	}
}

func TestUnit_Bridge_PermissionResolvedRetiresEveryCard(t *testing.T) {
	t.Run("an operator answer resolves as selected", func(t *testing.T) {
		h := newHarness(t)
		respCh := make(chan libacp.RequestPermissionResponse, 1)
		go func() {
			resp, err := h.bridge.client.RequestPermission(context.Background(), permissionRequest("beam-perm", "call-answer"))
			if err != nil {
				t.Errorf("RequestPermission: %v", err)
				return
			}
			respCh <- resp
		}()

		gate, ok := firstOfType[PermissionRequested](h.collect(10*time.Second, func(ev Event) bool {
			_, is := ev.(PermissionRequested)
			return is
		}))
		require.True(t, ok)
		gate.Resolve(true)

		done, ok := firstOfType[PermissionResolved](h.collect(10*time.Second, func(ev Event) bool {
			_, is := ev.(PermissionResolved)
			return is
		}))
		require.True(t, ok)
		require.Equal(t, libacp.SessionID("beam-perm"), done.SessionID)
		require.Equal(t, "call-answer", done.ToolCallID, "the card is matched on the tool call id")
		require.Equal(t, libacp.PermissionOutcomeSelected, done.Outcome)

		select {
		case resp := <-respCh:
			require.Equal(t, libacp.PermissionOutcomeSelected, resp.Outcome.Outcome)
		case <-time.After(10 * time.Second):
			t.Fatal("RequestPermission did not return after Resolve")
		}
	})

	t.Run("a cancelled request resolves as cancelled", func(t *testing.T) {
		h := newHarness(t)
		reqCtx, cancelReq := context.WithCancel(context.Background())
		defer cancelReq()

		respCh := make(chan libacp.RequestPermissionResponse, 1)
		go func() {
			resp, err := h.bridge.client.RequestPermission(reqCtx, permissionRequest("beam-perm", "call-cancelled"))
			if err != nil {
				t.Errorf("RequestPermission: %v", err)
				return
			}
			respCh <- resp
		}()

		h.collect(10*time.Second, func(ev Event) bool {
			_, is := ev.(PermissionRequested)
			return is
		})
		cancelReq()

		done, ok := firstOfType[PermissionResolved](h.collect(10*time.Second, func(ev Event) bool {
			_, is := ev.(PermissionResolved)
			return is
		}))
		require.True(t, ok)
		require.Equal(t, "call-cancelled", done.ToolCallID)
		require.Equal(t, libacp.PermissionOutcomeCancelled, done.Outcome)

		select {
		case resp := <-respCh:
			require.Equal(t, libacp.PermissionOutcomeCancelled, resp.Outcome.Outcome)
			require.Empty(t, resp.Outcome.OptionID)
		case <-time.After(10 * time.Second):
			t.Fatal("a cancelled permission request never returned")
		}
	})

	t.Run("teardown answers cancelled and closes the surface instead", func(t *testing.T) {
		h := newHarness(t)
		respCh := make(chan libacp.RequestPermissionResponse, 1)
		go func() {
			resp, err := h.bridge.client.RequestPermission(context.Background(), permissionRequest("beam-perm", "call-teardown"))
			if err != nil {
				t.Errorf("RequestPermission: %v", err)
				return
			}
			respCh <- resp
		}()

		h.collect(10*time.Second, func(ev Event) bool {
			_, is := ev.(PermissionRequested)
			return is
		})
		require.NoError(t, h.bridge.Close())

		select {
		case resp := <-respCh:
			require.Equal(t, libacp.PermissionOutcomeCancelled, resp.Outcome.Outcome)
		case <-time.After(10 * time.Second):
			t.Fatal("a pending permission request survived Close")
		}

		deadline := time.After(10 * time.Second)
		for {
			select {
			case ev, ok := <-h.bridge.Events():
				if !ok {
					return
				}
				_, is := ev.(PermissionResolved)
				require.False(t, is, "teardown drops queued events, including this one")
			case <-deadline:
				t.Fatal("the event channel was not closed by Close")
			}
		}
	})
}

func TestUnit_Bridge_CloseIsIdempotentAndClosesTheEventChannel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.initSession(ctx)

	require.NoError(t, h.bridge.Close())
	require.NoError(t, h.bridge.Close())

	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-h.bridge.Events():
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("event channel was not closed by Close")
		}
	}
closed:
	require.ErrorIs(t, h.bridge.SubmitPrompt("beam-x", "hi"), ErrClosed)
	require.ErrorIs(t, h.bridge.Cancel("beam-x"), ErrClosed)
	require.ErrorIs(t, h.bridge.RunShellLine("beam-x", "ls"), ErrClosed)
	_, err := h.bridge.Initialize(ctx)
	require.ErrorIs(t, err, ErrClosed)
	_, err = h.bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: scriptCwd})
	require.ErrorIs(t, err, ErrClosed)
	_, err = h.bridge.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: "beam-x", Cwd: scriptCwd})
	require.ErrorIs(t, err, ErrClosed)
	_, err = h.bridge.ResumeSession(ctx, libacp.ResumeSessionRequest{SessionID: "beam-x", Cwd: scriptCwd})
	require.ErrorIs(t, err, ErrClosed)
	_, err = h.bridge.ListSessions(ctx, libacp.ListSessionsRequest{Cwd: scriptCwd})
	require.ErrorIs(t, err, ErrClosed)
	_, err = h.bridge.CloseSession(ctx, libacp.CloseSessionRequest{SessionID: "beam-x"})
	require.ErrorIs(t, err, ErrClosed)
	_, err = h.bridge.DeleteSession(ctx, libacp.DeleteSessionRequest{SessionID: "beam-x"})
	require.ErrorIs(t, err, ErrClosed)
}

func TestUnit_Bridge_ContextCancellationClosesTheEventSurface(t *testing.T) {
	h := newHarness(t)
	_ = h.initSession(context.Background())

	h.cancel()

	deadline := time.After(20 * time.Second)
	for {
		select {
		case _, ok := <-h.bridge.Events():
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("cancelling the bridge context did not close the event channel")
		}
	}
closed:
	require.True(t, h.bridge.isClosed(), "a cancelled bridge must report itself closed")
	require.ErrorIs(t, h.bridge.SubmitPrompt("beam-x", "hi"), ErrClosed)
	require.NoError(t, h.bridge.Close(), "Close after cancellation still performs the join")
}

func TestUnit_Bridge_ConcurrentSubmitsDuringCloseAreSafe(t *testing.T) {
	h := newHarness(t)
	_ = h.initSession(context.Background())

	const submitters = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range submitters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			sid := libacp.SessionID(fmt.Sprintf("beam-race-%d", i))
			if err := h.bridge.SubmitPrompt(sid, "hi"); err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("submitter %d: unexpected error %v", i, err)
			}
			if err := h.bridge.RunShellLine(sid, "echo hi"); err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("submitter %d: unexpected shell error %v", i, err)
			}
		}(i)
	}

	close(start)
	require.NoError(t, h.bridge.Close(), "Close must join every admitted goroutine")
	wg.Wait()
}

func sessionTeardownDuringInflightPrompt(t *testing.T, teardown func(*harness, libacp.SessionID) error) {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	sid := h.initSession(ctx)

	entered := make(chan struct{})
	h.agent.setPrompt(func(pctx context.Context, _ libacp.PromptRequest) (libacp.PromptResponse, error) {
		close(entered)
		<-pctx.Done()
		return libacp.PromptResponse{StopReason: libacp.StopReasonCancelled}, nil
	})

	require.NoError(t, h.bridge.SubmitPrompt(sid, "long one"))
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent never received the prompt")
	}
	require.True(t, h.bridge.hasInflight(sid), "the turn must be in flight before teardown races it")

	teardownDone := make(chan error, 1)
	go func() { teardownDone <- teardown(h, sid) }()
	select {
	case err := <-teardownDone:
		require.NoError(t, err, "tearing a session down must not error while a prompt is in flight")
	case <-time.After(15 * time.Second):
		t.Fatal("session teardown did not return within 15s racing the in-flight prompt")
	}

	h.agent.waitForCancel(t, sid)

	events := h.collect(15*time.Second, func(ev Event) bool {
		switch ev.(type) {
		case TurnEnded, TurnFailed:
			return true
		}
		return false
	})
	ended, ok := firstOfType[TurnEnded](events)
	require.True(t, ok, "the torn-down session's turn never resolved: %+v", events)
	require.Equal(t, libacp.StopReasonCancelled, ended.StopReason)
	require.False(t, h.bridge.hasInflight(sid), "the torn-down session must release its in-flight mark")

	h.agent.setPrompt(nil)
	const fresh = libacp.SessionID("beam-fresh")
	require.NoError(t, h.bridge.SubmitPrompt(fresh, "hi"))
	events2 := h.collect(15*time.Second, func(ev Event) bool {
		_, ok := ev.(TurnEnded)
		return ok
	})
	ended2, ok := firstOfType[TurnEnded](events2)
	require.True(t, ok, "a fresh session on the same bridge must still complete a turn after the race")
	require.Equal(t, fresh, ended2.SessionID)
	require.Equal(t, libacp.StopReasonEndTurn, ended2.StopReason)
}

func TestUnit_Bridge_CloseSessionDuringInflightPrompt(t *testing.T) {
	sessionTeardownDuringInflightPrompt(t, func(h *harness, sid libacp.SessionID) error {
		_, err := h.bridge.CloseSession(context.Background(), libacp.CloseSessionRequest{SessionID: sid})
		require.Contains(t, h.agent.closedSessions(), sid)
		return err
	})
}

func TestUnit_Bridge_DeleteSessionDuringInflightPrompt(t *testing.T) {
	sessionTeardownDuringInflightPrompt(t, func(h *harness, sid libacp.SessionID) error {
		_, err := h.bridge.DeleteSession(context.Background(), libacp.DeleteSessionRequest{SessionID: sid})
		require.Contains(t, h.agent.deletedSessions(), sid)
		return err
	})
}

func TestUnit_Bridge_SessionTeardownWithoutAnInflightTurnSendsNoCancel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sid := h.initSession(ctx)

	_, err := h.bridge.CloseSession(ctx, libacp.CloseSessionRequest{SessionID: sid})
	require.NoError(t, err)
	_, err = h.bridge.DeleteSession(ctx, libacp.DeleteSessionRequest{SessionID: sid})
	require.NoError(t, err)

	require.Equal(t, []libacp.SessionID{sid}, h.agent.closedSessions())
	require.Equal(t, []libacp.SessionID{sid}, h.agent.deletedSessions())
	require.Empty(t, h.agent.cancelled(), "an idle session must not be cancelled on teardown")
}

func TestUnit_Bridge_ShellPassthroughReportsDisabledRuntime(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	require.NoError(t, h.bridge.RunShellLine(sid, "echo hi"))

	events := h.collect(15*time.Second, func(ev Event) bool {
		_, ok := ev.(ShellRunResult)
		return ok
	})
	started, ok := firstOfType[ShellRunStarted](events)
	require.True(t, ok, "the started event must precede the result")
	require.Equal(t, sid, started.SessionID)
	require.Equal(t, "echo hi", started.Command)

	res, ok := firstOfType[ShellRunResult](events)
	require.True(t, ok)
	require.Equal(t, sid, res.SessionID)
	require.ErrorIs(t, res.Err, ErrShellDisabled)
}

func TestUnit_Bridge_ShellPassthroughReportsTheRunResult(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	var gotMethod string
	var gotParams json.RawMessage
	h.agent.setExt(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
		gotMethod = method
		gotParams = params
		return json.RawMessage(`{"offset":42,"started":true,"output":"$ ls\nREADME.md\n"}`), nil
	})

	require.NoError(t, h.bridge.RunShellLine(sid, "ls"))

	events := h.collect(15*time.Second, func(ev Event) bool {
		_, ok := ev.(ShellRunResult)
		return ok
	})
	res, ok := firstOfType[ShellRunResult](events)
	require.True(t, ok)
	require.NoError(t, res.Err)
	require.EqualValues(t, 42, res.Offset)
	require.True(t, res.Started)
	require.Equal(t, "$ ls\nREADME.md\n", res.Snapshot)

	require.Equal(t, extMethodTerminalRun, gotMethod)
	require.JSONEq(t, fmt.Sprintf(`{"sessionId":%q,"command":"ls"}`, sid), string(gotParams))
}

func TestUnit_Bridge_ShellPassthroughReportsAMalformedResult(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	h.agent.setExt(func(context.Context, string, json.RawMessage) (json.RawMessage, *libacp.Error) {
		return json.RawMessage(`"not an object"`), nil
	})

	require.NoError(t, h.bridge.RunShellLine(sid, "ls"))

	events := h.collect(15*time.Second, func(ev Event) bool {
		_, ok := ev.(ShellRunResult)
		return ok
	})
	res, ok := firstOfType[ShellRunResult](events)
	require.True(t, ok)
	require.ErrorContains(t, res.Err, "decode terminal run result")
	require.NotErrorIs(t, res.Err, ErrShellDisabled, "a malformed reply is a failure, not an absent feature")
}

func TestUnit_Bridge_ShellPassthroughRejectsUnusableInput(t *testing.T) {
	h := newHarness(t)
	require.ErrorContains(t, h.bridge.RunShellLine("", "ls"), "session id is required")
	require.ErrorContains(t, h.bridge.RunShellLine("beam-x", ""), "empty shell line")
}

func TestUnit_Bridge_ActiveSessionFilterDropsOtherSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	note := func(sid libacp.SessionID, text string) libacp.SessionNotification {
		return libacp.SessionNotification{SessionID: sid, Update: libacp.NewAgentMessageChunk(text)}
	}

	h.bridge.SetActiveSession("s1")
	require.Equal(t, libacp.SessionID("s1"), h.bridge.ActiveSession())
	require.NoError(t, h.bridge.client.SessionUpdate(ctx, note("s2", "stale")))
	require.NoError(t, h.bridge.client.SessionUpdate(ctx, note("s1", "live")))

	h.bridge.SetActiveSession("s2")
	require.NoError(t, h.bridge.client.SessionUpdate(ctx, note("s1", "now-stale")))
	require.NoError(t, h.bridge.client.SessionUpdate(ctx, note("s2", "now-live")))

	h.bridge.SetActiveSession("")
	require.NoError(t, h.bridge.client.SessionUpdate(ctx, note("s3", "unfiltered")))

	var texts []string
	events := h.collect(10*time.Second, func(ev Event) bool {
		td, ok := ev.(TextDelta)
		return ok && td.Text == "unfiltered"
	})
	for _, ev := range events {
		texts = append(texts, ev.(TextDelta).Text)
	}
	require.Equal(t, []string{"live", "now-live", "unfiltered"}, texts)
}

func TestUnit_Bridge_EventsKeepWireOrder(t *testing.T) {
	h := newHarness(t)
	const sid = libacp.SessionID("s1")
	h.bridge.SetActiveSession(sid)

	const n = 200
	go func() {
		for i := range n {
			if err := h.agent.conn.SessionUpdate(libacp.SessionNotification{
				SessionID: sid,
				Update:    libacp.NewAgentMessageChunk(fmt.Sprintf("chunk-%d", i)),
			}); err != nil {
				t.Errorf("SessionUpdate %d: %v", i, err)
				return
			}
		}
	}()

	for i := range n {
		select {
		case ev := <-h.bridge.Events():
			td, ok := ev.(TextDelta)
			require.True(t, ok, "event %d was %T", i, ev)
			require.Equal(t, fmt.Sprintf("chunk-%d", i), td.Text)
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of %d events arrived", i, n)
		}
	}
}

func TestUnit_Bridge_FarSideClosingMidTurnFailsTheTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sid := h.initSession(ctx)

	entered := make(chan struct{})
	h.agent.setPrompt(func(pctx context.Context, _ libacp.PromptRequest) (libacp.PromptResponse, error) {
		close(entered)
		<-pctx.Done()
		return libacp.PromptResponse{}, pctx.Err()
	})

	require.NoError(t, h.bridge.SubmitPrompt(sid, "hello"))
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent never received the prompt")
	}

	require.NoError(t, h.agentSide.Close())

	events := h.collect(20*time.Second, func(ev Event) bool {
		switch ev.(type) {
		case TurnEnded, TurnFailed:
			return true
		}
		return false
	})
	failed, ok := firstOfType[TurnFailed](events)
	require.True(t, ok, "a turn cut off by the far side must surface as a failure, not silence: %+v", events)
	require.Equal(t, sid, failed.SessionID)
	require.Error(t, failed.Err)
	require.False(t, h.bridge.hasInflight(sid), "a severed turn must release its session")

	_, err := h.bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: scriptCwd})
	require.Error(t, err, "a call on a severed connection must answer, not hang")

	require.NoError(t, h.bridge.Close(), "a far-side hangup is not an unclean shutdown")
}

func TestUnit_Bridge_FarSideClosingBeforeATurnFailsTheSubmission(t *testing.T) {
	h := newHarness(t)
	sid := h.initSession(context.Background())

	require.NoError(t, h.agentSide.Close())

	require.NoError(t, h.bridge.SubmitPrompt(sid, "hello"))
	events := h.collect(20*time.Second, func(ev Event) bool {
		switch ev.(type) {
		case TurnEnded, TurnFailed:
			return true
		}
		return false
	})
	failed, ok := firstOfType[TurnFailed](events)
	require.True(t, ok, "submitting onto a dead connection must fail the turn: %+v", events)
	require.Error(t, failed.Err)
}

func eventTypeNames(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	require.NoError(t, err)

	var names []string
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Name.Name != "isBridgeEvent" {
			continue
		}
		ident, isIdent := fn.Recv.List[0].Type.(*ast.Ident)
		if !isIdent {
			continue
		}
		names = append(names, ident.Name)
	}
	sort.Strings(names)
	return names
}

func everyNotifiedUpdate() []libacp.SessionUpdate {
	userChunk := libacp.NewTextContent("typed")
	return []libacp.SessionUpdate{
		{SessionUpdate: libacp.SessionUpdateUserMessageChunk, Content: &userChunk, MessageID: "m1"},
		libacp.NewAgentMessageChunk("prose"),
		libacp.NewAgentThoughtChunk("reasoning"),
		{SessionUpdate: libacp.SessionUpdateToolCall, ToolCallID: "call-1", Title: "read", Kind: libacp.ToolKindRead},
		{SessionUpdate: libacp.SessionUpdateToolCallUpdate, ToolCallID: "call-1", Status: libacp.ToolCallStatusCompleted},
		{SessionUpdate: libacp.SessionUpdatePlan, Entries: []libacp.PlanEntry{{Content: "step"}}},
		{SessionUpdate: libacp.SessionUpdateAvailableCommands, AvailableCommands: []libacp.AvailableCommand{{Name: "help"}}},
		{SessionUpdate: libacp.SessionUpdateCurrentMode, CurrentModeID: "plan"},
		{SessionUpdate: libacp.SessionUpdateConfigOption, ConfigOptions: thinkOptions()},
		{SessionUpdate: libacp.SessionUpdateUsageUpdate, Used: 12, Size: 4096},
		{SessionUpdate: libacp.SessionUpdateSessionInfo, Title: "Fix the parser", UpdatedAt: "2026-07-27T10:00:00Z"},
		missionUpdate(missionReportMetaKey, map[string]any{"missionId": "mis-1", "reportId": "rep-1", "kind": "progress"}),
		missionUpdate(missionAskMetaKey, map[string]any{"missionId": "mis-1", "askId": "ask-1", "summary": "which branch?"}),
		missionUpdate(missionStatusMetaKey, map[string]any{"missionId": "mis-1", "oldStatus": "open", "newStatus": "landed"}),
		missionUpdate(missionPlanMetaKey, map[string]any{"missionId": "mis-1", "revision": 3, "entryCount": 6}),
		{
			SessionUpdate: dialect.TerminalOutputUpdateKind,
			Meta: mustMeta(map[string]any{dialect.TerminalOutputMetaKey: map[string]any{
				"sessionId": "beam-live", "offset": 7, "chunk": "$ ls\n",
			}}),
		},
		{SessionUpdate: libacp.SessionUpdateKind("_someone.future")},
	}
}

func missionUpdate(key string, payload map[string]any) libacp.SessionUpdate {
	update := libacp.NewAgentMessageChunk("mission traffic")
	update.Meta = mustMeta(map[string]any{key: payload})
	return update
}

func TestUnit_Bridge_EmitsEveryEventKind(t *testing.T) {
	want := eventTypeNames(t)
	require.NotEmpty(t, want, "the Event roster must come from events.go, not a hardcoded list")

	h := newHarness(t)
	ctx := context.Background()

	const live = libacp.SessionID("beam-live")
	const dead = libacp.SessionID("beam-dead")

	h.agent.setNewSession(func(libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
		return libacp.NewSessionResponse{SessionID: live, ConfigOptions: thinkOptions()}, nil
	})
	h.agent.setExt(func(context.Context, string, json.RawMessage) (json.RawMessage, *libacp.Error) {
		return json.RawMessage(`{"offset":7,"started":true,"output":"$ ls\n"}`), nil
	})
	h.agent.setPrompt(func(pctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
		if req.SessionID == dead {
			return libacp.PromptResponse{}, libacp.InternalError("unknown session")
		}
		for _, update := range everyNotifiedUpdate() {
			if err := h.agent.conn.SessionUpdate(libacp.SessionNotification{SessionID: live, Update: update}); err != nil {
				return libacp.PromptResponse{}, err
			}
		}
		if _, err := h.agent.conn.RequestPermission(pctx, permissionRequest(live, "call-gate")); err != nil {
			return libacp.PromptResponse{}, err
		}
		return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
	})

	_, err := h.bridge.Initialize(ctx)
	require.NoError(t, err)
	resp, err := h.bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: scriptCwd})
	require.NoError(t, err)
	require.Equal(t, live, resp.SessionID)
	h.bridge.SetActiveSession(live)

	_, err = h.bridge.LoadSession(ctx, libacp.LoadSessionRequest{SessionID: live, Cwd: scriptCwd})
	require.NoError(t, err)

	require.NoError(t, h.bridge.SubmitPrompt(live, "go"))
	require.NoError(t, h.bridge.SubmitPrompt(dead, "go"))
	require.NoError(t, h.bridge.RunShellLine(live, "ls"))
	h.inbox <- mustJSON(inboxItemJSON("inbox-1", "operator_fired", "result", "done"))

	seen := map[string]bool{}
	deadline := time.After(30 * time.Second)
	for len(seen) < len(want) {
		select {
		case ev, ok := <-h.bridge.Events():
			require.True(t, ok, "the event channel closed with %d of %d kinds seen", len(seen), len(want))
			seen[reflect.TypeOf(ev).Name()] = true
			if gate, is := ev.(PermissionRequested); is {
				gate.Resolve(true)
			}
		case <-deadline:
			var missing []string
			for _, name := range want {
				if !seen[name] {
					missing = append(missing, name)
				}
			}
			t.Fatalf("no path reaches these Event kinds: %v", missing)
		}
	}

	for _, name := range want {
		require.Truef(t, seen[name], "Event kind %s is declared but unreachable", name)
	}
}

func TestUnit_Translate_CoversEverySessionUpdateKind(t *testing.T) {
	const sid = libacp.SessionID("beam-1")

	textUpdate := func(kind libacp.SessionUpdateKind, text string) libacp.SessionUpdate {
		c := libacp.NewTextContent(text)
		return libacp.SessionUpdate{SessionUpdate: kind, Content: &c, MessageID: "m1"}
	}

	tests := []struct {
		name   string
		update libacp.SessionUpdate
		assert func(*testing.T, Event)
	}{
		{
			name:   "user_message_chunk",
			update: textUpdate(libacp.SessionUpdateUserMessageChunk, "typed"),
			assert: func(t *testing.T, ev Event) {
				e := requireType[UserEcho](t, ev)
				require.Equal(t, "typed", e.Text)
				require.Equal(t, "m1", e.MessageID)
			},
		},
		{
			name:   "agent_message_chunk",
			update: textUpdate(libacp.SessionUpdateAgentMessageChunk, "hello"),
			assert: func(t *testing.T, ev Event) {
				e := requireType[TextDelta](t, ev)
				require.Equal(t, "hello", e.Text)
				require.Equal(t, "m1", e.MessageID)
			},
		},
		{
			name:   "agent_thought_chunk",
			update: textUpdate(libacp.SessionUpdateAgentThoughtChunk, "thinking"),
			assert: func(t *testing.T, ev Event) {
				e := requireType[ThoughtDelta](t, ev)
				require.Equal(t, "thinking", e.Text)
			},
		},
		{
			name: "tool_call",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateToolCall,
				ToolCallID:    "call-1",
				Title:         "read file",
				Kind:          libacp.ToolKindRead,
				Status:        libacp.ToolCallStatusPending,
				Locations:     []libacp.ToolCallLocation{{Path: "/tmp/x"}},
				RawInput:      json.RawMessage(`{"path":"/tmp/x"}`),
				ToolContent:   []libacp.ToolCallContent{{Type: libacp.ToolCallContentRegular}},
				Meta:          mustMeta(map[string]any{"error": "policy denied"}),
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[ToolCallOpened](t, ev)
				require.Equal(t, "call-1", e.ToolCallID)
				require.Equal(t, libacp.ToolKindRead, e.Kind)
				require.Equal(t, libacp.ToolCallStatusPending, e.Status)
				require.Len(t, e.Locations, 1)
				require.Len(t, e.Contents, 1)
				require.JSONEq(t, `{"path":"/tmp/x"}`, string(e.RawInput))
				require.Equal(t, "policy denied", e.ErrorText)
			},
		},
		{
			name: "tool_call_update",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateToolCallUpdate,
				ToolCallID:    "call-1",
				Status:        libacp.ToolCallStatusCompleted,
				RawOutput:     json.RawMessage(`"done"`),
				Meta:          mustMeta(map[string]any{"error": "tool blew up"}),
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[ToolCallUpdated](t, ev)
				require.Equal(t, "call-1", e.ToolCallID)
				require.Equal(t, libacp.ToolCallStatusCompleted, e.Status)
				require.JSONEq(t, `"done"`, string(e.RawOutput))
				require.Equal(t, "tool blew up", e.ErrorText)
			},
		},
		{
			name: "plan",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdatePlan,
				Entries:       []libacp.PlanEntry{{Content: "step", Priority: libacp.PlanPriorityHigh, Status: libacp.PlanStatusPending}},
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[PlanUpdated](t, ev)
				require.Len(t, e.Entries, 1)
				require.Equal(t, "step", e.Entries[0].Content)
			},
		},
		{
			name: "available_commands_update",
			update: libacp.SessionUpdate{
				SessionUpdate:     libacp.SessionUpdateAvailableCommands,
				AvailableCommands: []libacp.AvailableCommand{{Name: "help", Description: "…"}},
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[CommandsUpdated](t, ev)
				require.Len(t, e.Commands, 1)
				require.Equal(t, "help", e.Commands[0].Name)
			},
		},
		{
			name: "current_mode_update",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateCurrentMode,
				CurrentModeID: "plan",
			},
			assert: func(t *testing.T, ev Event) {
				require.Equal(t, "plan", requireType[ModeUpdated](t, ev).ModeID)
			},
		},
		{
			name: "config_option_update",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateConfigOption,
				ConfigOptions: []libacp.SessionConfigOption{{ID: "model", Name: "Model"}},
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[ConfigOptionUpdated](t, ev)
				require.Len(t, e.Options, 1)
				require.Equal(t, "model", e.Options[0].ID)
			},
		},
		{
			name: "usage_update",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateUsageUpdate,
				Used:          12,
				Size:          4096,
				Cost:          &libacp.UsageCost{Amount: 0.5, Currency: "USD"},
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[UsageUpdated](t, ev)
				require.Equal(t, 12, e.Used)
				require.Equal(t, 4096, e.Size)
				require.NotNil(t, e.Cost)
			},
		},
		{
			name: "session_info_update",
			update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateSessionInfo,
				Title:         "Fix the parser",
				UpdatedAt:     "2026-07-27T10:00:00Z",
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[SessionInfoUpdated](t, ev)
				require.Equal(t, "Fix the parser", e.Title)
				require.Equal(t, "2026-07-27T10:00:00Z", e.UpdatedAt)
			},
		},
		{
			name: "terminal output extension kind",
			update: libacp.SessionUpdate{
				SessionUpdate: dialect.TerminalOutputUpdateKind,
				Meta: mustMeta(map[string]any{dialect.TerminalOutputMetaKey: map[string]any{
					"sessionId": "beam-1", "offset": 42, "chunk": "$ ls\n", "reset": false,
				}}),
			},
			assert: func(t *testing.T, ev Event) {
				e := requireType[TerminalChunk](t, ev)
				require.EqualValues(t, 42, e.Offset)
				require.Equal(t, "$ ls\n", e.Chunk)
				require.False(t, e.Reset)
			},
		},
		{
			name:   "unknown kind falls through instead of vanishing",
			update: libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateKind("_someone.future")},
			assert: func(t *testing.T, ev Event) {
				e := requireType[UnknownUpdate](t, ev)
				require.Equal(t, libacp.SessionUpdateKind("_someone.future"), e.Kind)
			},
		},
	}

	covered := map[libacp.SessionUpdateKind]bool{}
	for _, tt := range tests {
		covered[tt.update.SessionUpdate] = true
	}
	kinds := append(libacp.AllSessionUpdateKinds(), dialect.TerminalOutputUpdateKind)
	require.Greater(t, len(kinds), 1, "the library's kind roster must not be empty")
	for _, kind := range kinds {
		require.True(t, covered[kind], "session update kind %q has no translation case", kind)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := translate(libacp.SessionNotification{SessionID: sid, Update: tt.update})
			require.Equal(t, sid, ev.SessionOf(), "every event carries its session")
			tt.assert(t, ev)
		})
	}
}

func TestUnit_Translate_MissionEnvelopes(t *testing.T) {
	const sid = libacp.SessionID("beam-parent")

	t.Run("report", func(t *testing.T) {
		update := libacp.NewAgentMessageChunk("unit alpha reported (progress): done")
		update.MessageID = "mission-report-rep-1"
		update.Meta = mustMeta(map[string]any{missionReportMetaKey: map[string]any{
			"missionId": "mis-1", "reportId": "rep-1", "kind": "progress", "agentName": "alpha",
		}})

		rep := requireType[MissionReport](t, translate(libacp.SessionNotification{SessionID: sid, Update: update}))
		require.Equal(t, "mis-1", rep.MissionID)
		require.Equal(t, "rep-1", rep.ReportID)
		require.Equal(t, "progress", rep.Kind)
		require.Equal(t, "alpha", rep.AgentName)
		require.Equal(t, "mission-report-rep-1", rep.MessageID)
		require.Contains(t, rep.Text, "unit alpha reported")
	})

	t.Run("ask", func(t *testing.T) {
		update := libacp.NewAgentMessageChunk("unit alpha is WAITING: which branch?")
		update.MessageID = "mission-ask-ask-1"
		update.Meta = mustMeta(map[string]any{missionAskMetaKey: map[string]any{
			"missionId": "mis-1", "askId": "ask-1", "agentName": "alpha",
			"intent": "ship the fix", "summary": "which branch?", "detail": "main or release?",
		}})

		ask := requireType[MissionAsk](t, translate(libacp.SessionNotification{SessionID: sid, Update: update}))
		require.Equal(t, "mis-1", ask.MissionID)
		require.Equal(t, "ask-1", ask.AskID)
		require.Equal(t, "alpha", ask.AgentName)
		require.Equal(t, "ship the fix", ask.Intent)
		require.Equal(t, "which branch?", ask.Summary)
		require.Equal(t, "main or release?", ask.Detail)
		require.Equal(t, "mission-ask-ask-1", ask.MessageID)
	})

	t.Run("status change", func(t *testing.T) {
		update := libacp.NewAgentMessageChunk("unit alpha landed")
		update.MessageID = "mission-status-mis-1-open-landed"
		update.Meta = mustMeta(map[string]any{missionStatusMetaKey: map[string]any{
			"missionId": "mis-1", "agentName": "alpha", "intent": "ship the fix",
			"oldStatus": "open", "newStatus": "landed", "reason": "tests green",
		}})

		st := requireType[MissionStatusChanged](t, translate(libacp.SessionNotification{SessionID: sid, Update: update}))
		require.Equal(t, "mis-1", st.MissionID)
		require.Equal(t, "alpha", st.AgentName)
		require.Equal(t, MissionStatusOpen, st.Old)
		require.Equal(t, MissionStatusLanded, st.New)
		require.Equal(t, "tests green", st.Reason)
		require.Equal(t, "mission-status-mis-1-open-landed", st.MessageID)
	})

	t.Run("status change into open carries no prior status", func(t *testing.T) {
		update := libacp.NewAgentMessageChunk("unit alpha opened")
		update.Meta = mustMeta(map[string]any{missionStatusMetaKey: map[string]any{
			"missionId": "mis-1", "newStatus": "open",
		}})

		st := requireType[MissionStatusChanged](t, translate(libacp.SessionNotification{SessionID: sid, Update: update}))
		require.Empty(t, st.Old)
		require.Equal(t, MissionStatusOpen, st.New)
		require.False(t, MissionStatusTerminal(st.New))
	})

	t.Run("plan revision", func(t *testing.T) {
		update := libacp.NewAgentMessageChunk("unit alpha revised its plan")
		update.MessageID = "mission-plan-mis-1-3"
		update.Meta = mustMeta(map[string]any{missionPlanMetaKey: map[string]any{
			"missionId": "mis-1", "agentName": "alpha", "revision": 3,
			"explanation": "split the migration step", "entryCount": 6,
			"pending": 2, "inProgress": 1, "completed": 3,
		}})

		plan := requireType[MissionPlanRevised](t, translate(libacp.SessionNotification{SessionID: sid, Update: update}))
		require.Equal(t, "mis-1", plan.MissionID)
		require.Equal(t, "alpha", plan.AgentName)
		require.Equal(t, 3, plan.Revision)
		require.Equal(t, "split the migration step", plan.Explanation)
		require.Equal(t, 6, plan.EntryCount)
		require.Equal(t, 2, plan.Pending)
		require.Equal(t, 1, plan.InProgress)
		require.Equal(t, 3, plan.Completed)
		require.Equal(t, "mission-plan-mis-1-3", plan.MessageID)
	})

	t.Run("a foreign _meta namespace stays plain text", func(t *testing.T) {
		update := libacp.NewAgentMessageChunk("hello")
		update.Meta = mustMeta(map[string]any{"someone.else": map[string]any{"x": 1}})
		ev := translate(libacp.SessionNotification{SessionID: sid, Update: update})
		require.Equal(t, "hello", requireType[TextDelta](t, ev).Text)
	})

	t.Run("a malformed mission envelope is unknown, not prose", func(t *testing.T) {
		for _, key := range []string{
			missionReportMetaKey, missionAskMetaKey,
			missionStatusMetaKey, missionPlanMetaKey,
		} {
			update := libacp.NewAgentMessageChunk("unit alpha reported (progress): done")
			update.Meta = json.RawMessage(`{"` + key + `": "not-an-object"}`)
			unknown := requireType[UnknownUpdate](t, translate(libacp.SessionNotification{SessionID: sid, Update: update}))
			require.Equal(t, libacp.SessionUpdateAgentMessageChunk, unknown.Kind)
			require.Equal(t, sid, unknown.SessionID)
		}
	})
}

func TestUnit_MissionStatusTerminal(t *testing.T) {
	terminal := []string{MissionStatusLanded, MissionStatusDerailed, MissionStatusStuck, MissionStatusAbandoned}
	for _, s := range terminal {
		require.True(t, MissionStatusTerminal(s), "%q must be terminal", s)
	}
	for _, s := range []string{"", MissionStatusOpen, "paused", "LANDED", "landed "} {
		require.False(t, MissionStatusTerminal(s), "%q must not be terminal", s)
	}
}

func TestUnit_Translate_TerminalChunkReset(t *testing.T) {
	tests := []struct {
		name   string
		meta   json.RawMessage
		assert func(*testing.T, Event)
	}{
		{
			name: "reset snapshot",
			meta: mustMeta(map[string]any{dialect.TerminalOutputMetaKey: map[string]any{
				"sessionId": "beam-1", "offset": 0, "chunk": "scrollback", "reset": true,
			}}),
			assert: func(t *testing.T, ev Event) {
				e := requireType[TerminalChunk](t, ev)
				require.True(t, e.Reset)
				require.Equal(t, "scrollback", e.Chunk)
			},
		},
		{
			name: "missing payload does not fabricate an empty reset",
			meta: mustMeta(map[string]any{"someone.else": map[string]any{}}),
			assert: func(t *testing.T, ev Event) {
				require.Equal(t, dialect.TerminalOutputUpdateKind, requireType[UnknownUpdate](t, ev).Kind)
			},
		},
		{
			name: "malformed payload does not fabricate an empty reset",
			meta: json.RawMessage(`{"` + dialect.TerminalOutputMetaKey + `": "not-an-object"}`),
			assert: func(t *testing.T, ev Event) {
				require.Equal(t, dialect.TerminalOutputUpdateKind, requireType[UnknownUpdate](t, ev).Kind)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := translate(libacp.SessionNotification{
				SessionID: "beam-1",
				Update:    libacp.SessionUpdate{SessionUpdate: dialect.TerminalOutputUpdateKind, Meta: tt.meta},
			})
			tt.assert(t, ev)
		})
	}
}

func TestUnit_Translate_SurvivesAbsentContent(t *testing.T) {
	for _, kind := range []libacp.SessionUpdateKind{
		libacp.SessionUpdateUserMessageChunk,
		libacp.SessionUpdateAgentMessageChunk,
		libacp.SessionUpdateAgentThoughtChunk,
	} {
		ev := translate(libacp.SessionNotification{
			SessionID: "beam-1",
			Update:    libacp.SessionUpdate{SessionUpdate: kind},
		})
		require.NotNil(t, ev)
		require.Equal(t, libacp.SessionID("beam-1"), ev.SessionOf())
	}
}

func TestUnit_SelectedModel(t *testing.T) {
	tests := []struct {
		name         string
		options      []libacp.SessionConfigOption
		wantProvider string
		wantModel    string
		wantOK       bool
	}{
		{
			name:         "provider and model",
			options:      []libacp.SessionConfigOption{{ID: "model", CurrentValue: "openai/gpt-5"}},
			wantProvider: "openai",
			wantModel:    "gpt-5",
			wantOK:       true,
		},
		{
			name:      "a model name may carry further slashes",
			options:   []libacp.SessionConfigOption{{ID: "model", CurrentValue: "ollama/library/qwen3:8b"}},
			wantModel: "library/qwen3:8b", wantProvider: "ollama", wantOK: true,
		},
		{
			name:      "an ungrouped value is a model with no provider",
			options:   []libacp.SessionConfigOption{{ID: "model", CurrentValue: "gpt-5"}},
			wantModel: "gpt-5", wantOK: true,
		},
		{
			name:    "an empty selection is not an answer",
			options: []libacp.SessionConfigOption{{ID: "model", CurrentValue: "  "}},
		},
		{
			name:    "no model select at all",
			options: []libacp.SessionConfigOption{{ID: "think", CurrentValue: "high"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, model, ok := SelectedModel(tt.options)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantProvider, provider)
			require.Equal(t, tt.wantModel, model)
		})
	}
}

func TestUnit_ClassifyShellError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"method not found is absence", libacp.MethodNotFound(extMethodTerminalRun), ErrShellDisabled},
		{"internal error passes through", libacp.InternalError("boom"), libacp.InternalError("boom")},
		{"untyped error passes through", context.Canceled, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyShellError(tt.err)
			if tt.want == ErrShellDisabled {
				require.ErrorIs(t, got, ErrShellDisabled)
				return
			}
			require.Equal(t, tt.want.Error(), got.Error())
		})
	}
}

func TestUnit_IsShutdownNoise(t *testing.T) {
	require.True(t, isShutdownNoise(nil))
	require.True(t, isShutdownNoise(context.Canceled))
	require.True(t, isShutdownNoise(io.EOF))
	require.True(t, isShutdownNoise(io.ErrClosedPipe))
	require.True(t, isShutdownNoise(libacp.ErrConnectionClosed))
	require.True(t, isShutdownNoise(fmt.Errorf("wrapped: %w", io.ErrClosedPipe)))
	require.False(t, isShutdownNoise(fmt.Errorf("real failure")))
	require.False(t, isShutdownNoise(context.DeadlineExceeded))
}
