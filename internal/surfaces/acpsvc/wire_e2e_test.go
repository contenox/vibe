package acpsvc

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func taskEventForTool(toolName, taskID string) taskengine.TaskEvent {
	return taskengine.TaskEvent{ToolName: toolName, TaskID: taskID}
}

// wirePipe adapts one end of an io.Pipe pair to io.ReadWriteCloser.
type wirePipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *wirePipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *wirePipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *wirePipe) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

type wireClient struct {
	t    *testing.T
	rw   io.ReadWriteCloser
	buf  []byte
	next int64
}

func (c *wireClient) call(method string, params any) (libacp.Response, []libacp.Notification) {
	c.t.Helper()
	c.next++
	raw, err := json.Marshal(params)
	require.NoError(c.t, err)
	req := libacp.NewRequest(libacp.NewRequestIDNumber(c.next), method, raw)
	line, err := json.Marshal(req)
	require.NoError(c.t, err)
	_, err = c.rw.Write(append(line, '\n'))
	require.NoError(c.t, err)

	// Collect notifications until the response for this id arrives, so callers
	// can assert ordering (everything before was written before the response).
	var notes []libacp.Notification
	for {
		in := c.read()
		switch in.Kind {
		case libacp.IncomingKindNotification:
			notes = append(notes, in.Notification)
		case libacp.IncomingKindResponse:
			require.Equal(c.t, libacp.NewRequestIDNumber(c.next), in.Response.ID)
			return in.Response, notes
		default:
			c.t.Fatalf("unexpected incoming kind %d", in.Kind)
		}
	}
}

// drainNotifications reads notifications already flushed after a response
// (AfterResponse ordering) until `want` have been seen or the deadline hits.
func (c *wireClient) drainNotifications(want int) []libacp.Notification {
	c.t.Helper()
	// Generous deadline: notifications come from a spawned subprocess, and
	// full-suite load can far exceed an isolated run's roundtrip time.
	var notes []libacp.Notification
	deadline := time.After(30 * time.Second)
	done := make(chan libacp.Incoming, want)
	go func() {
		for i := 0; i < want; i++ {
			done <- c.read()
		}
	}()
	for len(notes) < want {
		select {
		case in := <-done:
			require.Equal(c.t, libacp.IncomingKindNotification, in.Kind)
			notes = append(notes, in.Notification)
		case <-deadline:
			c.t.Fatalf("timed out waiting for %d notifications (got %d)", want, len(notes))
		}
	}
	return notes
}

func (c *wireClient) read() libacp.Incoming {
	c.t.Helper()
	tmp := make([]byte, 4096)
	for {
		for i, b := range c.buf {
			if b == '\n' {
				line := append([]byte{}, c.buf[:i]...)
				c.buf = append([]byte{}, c.buf[i+1:]...)
				in, err := libacp.ParseIncoming(line)
				require.NoError(c.t, err, "wire: %s", line)
				return in
			}
		}
		n, err := c.rw.Read(tmp)
		if n > 0 {
			c.buf = append(c.buf, tmp[:n]...)
		}
		require.NoError(c.t, err)
	}
}

// TestE2E_Wire_SessionNewListLoadRoundTrip pins that every id session/list returns is loadable by session/load, and list reports the session's cwd.
func TestE2E_Wire_SessionNewListLoadRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factory := New(Deps{
		// A bare engine is enough for the session lifecycle; prompting isn't
		// exercised here.
		Engine:      &enginesvc.Engine{},
		DB:          db,
		WorkspaceID: "wire-test-ws",
	})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		return factory(c)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()
	defer func() {
		_ = clientSide.Close()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Error("connection did not shut down")
		}
	}()

	client := &wireClient{t: t, rw: clientSide}

	resp, _ := client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)
	var initResp libacp.InitializeResponse
	require.NoError(t, json.Unmarshal(resp.Result, &initResp))
	require.Equal(t, libacp.ProtocolVersion, initResp.ProtocolVersion)
	require.NotNil(t, initResp.AgentCapabilities.SessionCapabilities.List, "session/list capability must be advertised")

	cwd := t.TempDir()
	resp, notes := client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)
	assert.Empty(t, notes, "no session/update may precede the session/new result (Zed drops updates for unknown sessions)")
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	require.NotEmpty(t, newResp.SessionID)

	// The deferred available_commands_update must follow the result.
	after := client.drainNotifications(1)
	require.Equal(t, libacp.MethodSessionUpdate, after[0].Method)
	var cmdNote libacp.SessionNotification
	require.NoError(t, json.Unmarshal(after[0].Params, &cmdNote))
	assert.Equal(t, libacp.SessionUpdateAvailableCommands, cmdNote.Update.SessionUpdate)
	assert.Equal(t, newResp.SessionID, cmdNote.SessionID)

	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{})
	require.Nil(t, resp.Error)
	var listResp libacp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(resp.Result, &listResp))
	require.Len(t, listResp.Sessions, 1)
	assert.Equal(t, newResp.SessionID, listResp.Sessions[0].SessionID,
		"list must return the loadable session id (mi.name), not the internal UUID")
	assert.Equal(t, cwd, listResp.Sessions[0].Cwd, "list must report the session's cwd")
	assert.Equal(t, string(newResp.SessionID), listResp.Sessions[0].Title,
		"title falls back to the session name when there is no first user message yet")

	// Insert through chatservice against the internal id (mi.id), distinct
	// from the ACP-level session id (mi.name) session/new returned.
	var internalSessionID string
	require.NoError(t, db.WithoutTransaction().QueryRowContext(ctx,
		`SELECT id FROM message_indices WHERE name = $1 AND workspace_id = $2 AND identity = 'acp-client'`,
		string(newResp.SessionID), "wire-test-ws",
	).Scan(&internalSessionID))
	longFirstMessage := "   this   is the very first user message and it runs on for a good long while past sixty characters   "
	require.NoError(t, chatservice.NewManager("wire-test-ws").PersistDiff(ctx, db.WithoutTransaction(), internalSessionID, []taskengine.Message{
		{Role: "user", Content: longFirstMessage, Timestamp: time.Now()},
		{Role: "assistant", Content: "an unrelated reply", Timestamp: time.Now().Add(time.Second)},
	}))
	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{})
	require.Nil(t, resp.Error)
	require.NoError(t, json.Unmarshal(resp.Result, &listResp))
	require.Len(t, listResp.Sessions, 1)
	wantTitle := strings.Join(strings.Fields(longFirstMessage), " ")
	wantTitle = string([]rune(wantTitle)[:57]) + "..."
	assert.Equal(t, wantTitle, listResp.Sessions[0].Title,
		"title must be the first user message, whitespace-collapsed and truncated to 60 runes, not the last (assistant) message")
	assert.LessOrEqual(t, len([]rune(listResp.Sessions[0].Title)), 60, "title must not exceed the 60-rune budget")

	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{Cwd: "/somewhere/else"})
	require.Nil(t, resp.Error)
	require.NoError(t, json.Unmarshal(resp.Result, &listResp))
	assert.Empty(t, listResp.Sessions)

	resp, notes = client.call(libacp.MethodSessionLoad, libacp.LoadSessionRequest{
		SessionID:  newResp.SessionID,
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error, "session/load must resolve every id session/list returned")
	for _, n := range notes {
		assert.Equal(t, libacp.MethodSessionUpdate, n.Method, "history replay precedes the load result")
	}
	client.drainNotifications(1) // deferred available_commands_update after load

	require.NotNil(t, initResp.AgentCapabilities.SessionCapabilities.Resume, "resume capability must be advertised")
	resp, notes = client.call(libacp.MethodSessionResume, libacp.ResumeSessionRequest{
		SessionID: newResp.SessionID,
		Cwd:       cwd,
	})
	require.Nil(t, resp.Error)
	assert.Empty(t, notes, "session/resume must not replay history")
	// Deferred after resume: available_commands_update, then usage_update
	// re-seeding the context gauge (resume doesn't hand that back with history).
	client.drainNotifications(2)

	require.NotNil(t, initResp.AgentCapabilities.SessionCapabilities.Close, "close capability must be advertised")
	resp, _ = client.call(libacp.MethodSessionClose, libacp.CloseSessionRequest{SessionID: newResp.SessionID})
	require.Nil(t, resp.Error)
	resp, _ = client.call(libacp.MethodSessionClose, libacp.CloseSessionRequest{SessionID: newResp.SessionID})
	require.Nil(t, resp.Error, "closing an already-closed session succeeds")

	require.NotNil(t, initResp.AgentCapabilities.SessionCapabilities.Delete, "delete capability must be advertised")
	resp, _ = client.call(libacp.MethodSessionDelete, libacp.DeleteSessionRequest{SessionID: newResp.SessionID})
	require.Nil(t, resp.Error)
	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{})
	require.Nil(t, resp.Error)
	require.NoError(t, json.Unmarshal(resp.Result, &listResp))
	assert.Empty(t, listResp.Sessions, "deleted sessions must not be listed")
	resp, _ = client.call(libacp.MethodSessionDelete, libacp.DeleteSessionRequest{SessionID: newResp.SessionID})
	require.Nil(t, resp.Error, "spec: deleting a nonexistent session SHOULD succeed silently")
}

// TestE2E_Wire_SessionListPagination pins the cursor contract: pages are bounded, nextCursor resumes exactly where the previous page ended, and no session is duplicated, skipped, or missing.
func TestE2E_Wire_SessionListPagination(t *testing.T) {
	prev := listSessionsPageSize
	listSessionsPageSize = 2
	t.Cleanup(func() { listSessionsPageSize = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-paging.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}
	factory := New(Deps{Engine: &enginesvc.Engine{}, DB: db, WorkspaceID: "paging-ws"})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent { return factory(c) })
	go func() { _ = conn.Run(ctx) }()
	defer clientSide.Close()

	client := &wireClient{t: t, rw: clientSide}
	resp, _ := client.call(libacp.MethodInitialize, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.Nil(t, resp.Error)

	created := map[libacp.SessionID]bool{}
	for i := 0; i < 5; i++ {
		resp, _ := client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
		require.Nil(t, resp.Error)
		var nr libacp.NewSessionResponse
		require.NoError(t, json.Unmarshal(resp.Result, &nr))
		created[nr.SessionID] = true
		client.drainNotifications(1) // available_commands_update
	}

	seen := map[libacp.SessionID]int{}
	cursor := ""
	pages := 0
	for {
		resp, _ := client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{Cursor: cursor})
		require.Nil(t, resp.Error)
		var list libacp.ListSessionsResponse
		require.NoError(t, json.Unmarshal(resp.Result, &list))
		require.LessOrEqual(t, len(list.Sessions), 2, "pages must respect the page size")
		for _, s := range list.Sessions {
			seen[s.SessionID]++
		}
		pages++
		require.LessOrEqual(t, pages, 10, "paging must terminate")
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	require.Equal(t, 3, pages, "5 sessions at page size 2 = 3 pages")
	require.Len(t, seen, 5, "every session appears")
	for sid, n := range seen {
		require.Equal(t, 1, n, "session %s must appear exactly once across pages", sid)
		require.True(t, created[sid])
	}
}

// TestUnit_Initialize_AdvertisesEnvVarAuth_InSetupOnlyMode pins that with no engine and an EnvSetup spec, initialize offers the env_var auth method with its variable contract.
func TestUnit_Initialize_AdvertisesEnvVarAuth_InSetupOnlyMode(t *testing.T) {
	tr := &Transport{
		deps: Deps{EnvSetup: &EnvSetupSpec{
			Vars: []libacp.AuthEnvVar{{Name: "CONTENOX_DEFAULT_PROVIDER"}},
		}},
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
	}
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	var envMethod *libacp.AuthMethod
	for i := range resp.AuthMethods {
		if resp.AuthMethods[i].Type == libacp.AuthMethodTypeEnvVar {
			envMethod = &resp.AuthMethods[i]
		}
	}
	require.NotNil(t, envMethod, "setup-only mode must advertise the env_var auth method")
	assert.Equal(t, "env", envMethod.ID)
	require.Len(t, envMethod.Vars, 1)
	assert.Equal(t, "CONTENOX_DEFAULT_PROVIDER", envMethod.Vars[0].Name)
}

// TestUnit_Initialize_AdvertisesTerminalSetup pins that terminal-auth-capable clients are offered the terminal setup method (`acp --setup`), while the retired browser method is deliberately not advertised.
func TestUnit_Initialize_AdvertisesTerminalSetup(t *testing.T) {
	tr := transportWithMeta(`{"terminal-auth":true}`)
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{Meta: tr.clientCaps.Meta},
	})
	require.NoError(t, err)

	byID := map[string]libacp.AuthMethod{}
	for _, m := range resp.AuthMethods {
		byID[m.ID] = m
	}
	terminal, ok := byID["terminal"]
	require.True(t, ok, "terminal setup method must be advertised")
	assert.Equal(t, libacp.AuthMethodTypeTerminal, terminal.Type)
	assert.Contains(t, terminal.Args, "--setup")

	_, ok = byID["browser"]
	assert.False(t, ok, "browser setup method was retired and must not be advertised")

	_, err = tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: "terminal"})
	require.NoError(t, err, "authenticate must accept the advertised terminal method")
}

// TestUnit_Initialize_AdvertisesWorkspaceConfigOptions pins that a configured agent advertises workspace-level model/think/HITL/token-limit options in initialize's _meta, before any session is minted.
func TestUnit_Initialize_AdvertisesWorkspaceConfigOptions(t *testing.T) {
	tr := &Transport{
		deps:            Deps{Engine: &enginesvc.Engine{}},
		sessions:        make(map[libacp.SessionID]*sessionEntry),
		contenoxToACPID: make(map[string]libacp.SessionID),
		defaultProvider: "openai",
		defaultModel:    "gpt-5-mini",
		defaultThink:    "medium",
	}
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	require.NotNil(t, resp.Meta, "configured agent must advertise workspace config options")

	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp.Meta, &meta))
	raw, ok := meta[WorkspaceConfigOptionsMetaKey]
	require.True(t, ok, "initialize _meta must carry %q", WorkspaceConfigOptionsMetaKey)

	var options []libacp.SessionConfigOption
	require.NoError(t, json.Unmarshal(raw, &options))
	require.Len(t, options, 4)
	require.Equal(t, "openai/gpt-5-mini", optionByID(t, options, configIDModel).CurrentValue)
	require.Equal(t, "medium", optionByID(t, options, configIDThink).CurrentValue)
}

// TestUnit_Initialize_OmitsWorkspaceConfigOptions_InSetupOnlyMode pins that an unconfigured agent (no engine) advertises no workspace config options.
func TestUnit_Initialize_OmitsWorkspaceConfigOptions_InSetupOnlyMode(t *testing.T) {
	tr := transportWithMeta("")
	resp, err := tr.Initialize(context.Background(), libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	if resp.Meta == nil {
		return
	}
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp.Meta, &meta))
	_, ok := meta[WorkspaceConfigOptionsMetaKey]
	require.False(t, ok, "setup-only agent must not advertise workspace config options")
}

func TestUnit_Authenticate_EnvMethod(t *testing.T) {
	completed := 0
	tr := &Transport{
		deps: Deps{EnvSetup: &EnvSetupSpec{
			Complete: func(context.Context) error { completed++; return nil },
		}},
	}

	_, err := tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: "env"})
	require.NoError(t, err)
	assert.Equal(t, 1, completed)

	// Failure surfaces as auth_required with the reason, so the client can show
	// what is missing.
	tr.deps.EnvSetup.Complete = func(context.Context) error {
		return assert.AnError
	}
	_, err = tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: "env"})
	require.Error(t, err)
	var e *libacp.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, libacp.ErrAuthRequired, e.Code)

	// Once configured (engine present), the method is no longer advertised and
	// must be rejected.
	tr.deps.Engine = &enginesvc.Engine{}
	_, err = tr.Authenticate(context.Background(), libacp.AuthenticateRequest{MethodID: "env"})
	require.Error(t, err)
	require.ErrorAs(t, err, &e)
	assert.Equal(t, libacp.ErrInvalidParams, e.Code)
}

// TestE2E_Wire_SessionListOrder pins that session/list sorts by last message activity descending (never by internal UUID), never-messaged sessions last, each with a parseable UpdatedAt.
func TestE2E_Wire_SessionListOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-order.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}
	factory := New(Deps{Engine: &enginesvc.Engine{}, DB: db, WorkspaceID: "order-ws"})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent { return factory(c) })
	go func() { _ = conn.Run(ctx) }()
	defer clientSide.Close()

	client := &wireClient{t: t, rw: clientSide}
	resp, _ := client.call(libacp.MethodInitialize, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.Nil(t, resp.Error)

	var sids []libacp.SessionID
	for i := 0; i < 4; i++ {
		resp, _ := client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []libacp.McpServer{}})
		require.Nil(t, resp.Error)
		var nr libacp.NewSessionResponse
		require.NoError(t, json.Unmarshal(resp.Result, &nr))
		sids = append(sids, nr.SessionID)
		client.drainNotifications(1) // available_commands_update
	}

	// Backdate activity out of creation order via the message store, the exact
	// production path whose stored time format the list must parse back.
	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), "order-ws")
	base := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	activity := map[int]time.Time{
		0: base.Add(1 * time.Hour), // middle
		1: base,                    // oldest
		2: base.Add(2 * time.Hour), // freshest
	}
	for i, at := range activity {
		info, err := store.GetMessageSessionByName(ctx, "acp-client", string(sids[i]))
		require.NoError(t, err)
		require.NoError(t, store.AppendMessages(ctx, &runtimetypes.Message{
			ID:      string(sids[i]) + "-m1",
			IDX:     info.ID,
			Payload: []byte(`{"role":"user","content":"hello"}`),
			AddedAt: at,
		}))
	}

	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{})
	require.Nil(t, resp.Error)
	var list libacp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(resp.Result, &list))
	require.Len(t, list.Sessions, 4)

	var got []libacp.SessionID
	for _, s := range list.Sessions {
		got = append(got, s.SessionID)
	}
	assert.Equal(t, []libacp.SessionID{sids[2], sids[0], sids[1], sids[3]}, got,
		"freshest activity first, never-messaged session last")
	for _, s := range list.Sessions[:3] {
		_, err := time.Parse(time.RFC3339, s.UpdatedAt)
		assert.NoError(t, err, "messaged session %s must carry a parseable UpdatedAt (got %q)", s.SessionID, s.UpdatedAt)
	}
}

// TestUnit_ToolCallWireID_DisambiguatesRepeatedInvocations pins that, without an ApprovalID, repeated runs of one tool get distinct wire ids while a pending/result pair of one invocation shares its id.
func TestUnit_ToolCallWireID_DisambiguatesRepeatedInvocations(t *testing.T) {
	tr := &Transport{}
	sid := libacp.SessionID("s1")
	ev := taskEventForTool("web_search", "task-1")

	// Invocation 1: pending opens, result closes with the same id.
	first := tr.toolCallWireID(sid, ev, false)
	assert.Equal(t, "web_search", first, "first invocation keeps the bare name (wire-compatible with prior behavior)")
	assert.Equal(t, first, tr.toolCallWireID(sid, ev, true), "result must correlate with its pending card")

	// Invocation 2 of the same tool: a distinct card.
	second := tr.toolCallWireID(sid, ev, false)
	assert.NotEqual(t, first, second, "repeated invocations must not merge into one card")
	assert.Equal(t, second, tr.toolCallWireID(sid, ev, true))

	// A result without any pending (declarative tools path) is its own invocation.
	third := tr.toolCallWireID(sid, ev, true)
	assert.NotEqual(t, second, third)

	// ApprovalID short-circuits everything.
	withApproval := taskEventForTool("web_search", "task-1")
	withApproval.ApprovalID = "appr-42"
	assert.Equal(t, "appr-42", tr.toolCallWireID(sid, withApproval, false))

	// Session isolation: another session starts fresh.
	assert.Equal(t, "web_search", tr.toolCallWireID(libacp.SessionID("s2"), ev, false))

	// clearToolCallState resets the counters for the session.
	tr.clearToolCallState(sid)
	assert.Equal(t, "web_search", tr.toolCallWireID(sid, ev, false))
}

// TestE2E_Wire_SessionWorkspaceCwd pins the Session Workspaces contract: a non-allowlisted cwd is refused, "/" resolves to the default root, an allowlisted root is accepted and reported, and the serve cwd resolver roots agent tools at the session's chosen workspace.
func TestE2E_Wire_SessionWorkspaceCwd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-ws.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	rootA := t.TempDir() // default root
	rootB := t.TempDir() // second allowlisted root
	require.NoError(t, os.WriteFile(filepath.Join(rootA, "only-in-a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootB, "hello-b.txt"), []byte("b"), 0o644))

	factory, err := vfs.NewFactory(rootA, rootB)
	require.NoError(t, err)
	resolvedA := factory.Default()
	resolvedB, ok := factory.Allows(rootB)
	require.True(t, ok)

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factoryFn := New(Deps{
		Engine:         &enginesvc.Engine{},
		DB:             db,
		WorkspaceID:    "wire-ws",
		WorkspaceRoots: factory,
	})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		return factoryFn(c)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()
	defer func() {
		_ = clientSide.Close()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Error("connection did not shut down")
		}
	}()

	client := &wireClient{t: t, rw: clientSide}
	resp, _ := client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	// The empty chat learns the allowlist from the workspace config options _meta.
	var initResp libacp.InitializeResponse
	require.NoError(t, json.Unmarshal(resp.Result, &initResp))
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(initResp.Meta, &meta))
	var wsOptions []libacp.SessionConfigOption
	require.NoError(t, json.Unmarshal(meta[WorkspaceConfigOptionsMetaKey], &wsOptions))
	rootOption := optionByID(t, wsOptions, configIDWorkspaceRoot)
	assert.Equal(t, resolvedA, rootOption.CurrentValue, "default root is the current workspace value")
	var rootValues []string
	for _, v := range rootOption.Options.AllValues() {
		rootValues = append(rootValues, v.Value)
	}
	assert.ElementsMatch(t, []string{resolvedA, resolvedB}, rootValues, "picker must list every allowlisted root")

	// 1. A non-allowlisted cwd is refused.
	resp, _ = client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        "/definitely/not/allowed",
		McpServers: []libacp.McpServer{},
	})
	require.NotNil(t, resp.Error, "a cwd outside the allowlist must be rejected")
	assert.Equal(t, libacp.ErrInvalidParams, resp.Error.Code)

	// 2. The "/" sentinel resolves to the default root (compat with beam).
	resp, _ = client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        "/",
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)
	var defResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &defResp))
	client.drainNotifications(1)
	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{})
	require.Nil(t, resp.Error)
	var listResp libacp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(resp.Result, &listResp))
	found := false
	for _, s := range listResp.Sessions {
		if s.SessionID == defResp.SessionID {
			found = true
			assert.Equal(t, resolvedA, s.Cwd, `"/" must resolve to the default root`)
		}
	}
	assert.True(t, found)

	// 3. An allowlisted root is accepted and reported.
	resp, _ = client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        rootB,
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)
	var bResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &bResp))
	client.drainNotifications(1)

	// 4. The serve cwd resolver roots local_fs at the session's chosen workspace.
	var internalID string
	require.NoError(t, db.WithoutTransaction().QueryRowContext(ctx,
		`SELECT id FROM message_indices WHERE name = $1 AND identity = 'acp-client'`,
		string(bResp.SessionID),
	).Scan(&internalID))

	resolverTracker := &recTracker{}
	resolver := NewServeCwdResolver(db, factory, resolverTracker)
	toolCtx := context.WithValue(ctx, runtimetypes.SessionIDContextKey, internalID)
	assert.Equal(t, resolvedB, resolver(toolCtx), "resolver must return the session's persisted cwd")

	// A session without a workspace in scope falls back to the default root.
	assert.Equal(t, factory.Default(), resolver(context.Background()))

	// Neither resolution above was a refusal, so nothing should have been reported.
	assert.Nil(t, resolverTracker.findSubject("resolve", "session_workspace"),
		"an in-allowlist resolution must not report a workspace degradation")

	// 4b. A persisted cwd is re-checked against the current allowlist, not
	// trusted because it was allowlisted when written: a serve started with
	// narrower roots must not hand rootB to the agent's tools or PTY.
	narrowed, err := vfs.NewFactory(rootA)
	require.NoError(t, err)
	narrowedTracker := &recTracker{}
	narrowedResolver := NewServeCwdResolver(db, narrowed, narrowedTracker)
	assert.Equal(t, resolvedA, narrowedResolver(toolCtx),
		"a stored cwd outside the current allowlist must fall back to the default root, not be adopted verbatim")

	// The fallback is silent to the agent (the resolver has no error channel),
	// so the operator's only sight of it is this report; assert the whole span.
	degraded := narrowedTracker.findSubject("resolve", "session_workspace")
	require.NotNil(t, degraded, "a rejected stored cwd must be reported through the tracker")
	require.Equal(t, 1, degraded.errs, "the refusal must be reported exactly once")
	require.Equal(t, 1, degraded.ended, "the span must be closed")
	require.ErrorContains(t, degraded.lastErr, "outside the configured workspace roots",
		"the report must carry the reason the stored cwd was refused")
	assert.Equal(t, resolvedB, degraded.kvStr("stored_cwd"), "the report must name the refused cwd")
	assert.Equal(t, narrowed.Default(), degraded.kvStr("default_root"), "the report must name the root used instead")

	// 5. Reloading with "/" (what beam sends) must preserve the session's
	// workspace, not reset it to the default root.
	resp, _ = client.call(libacp.MethodSessionLoad, libacp.LoadSessionRequest{
		SessionID:  bResp.SessionID,
		Cwd:        "/",
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)
	client.drainNotifications(1) // deferred available_commands_update after load
	resp, _ = client.call(libacp.MethodSessionList, libacp.ListSessionsRequest{})
	require.Nil(t, resp.Error)
	require.NoError(t, json.Unmarshal(resp.Result, &listResp))
	for _, s := range listResp.Sessions {
		if s.SessionID == bResp.SessionID {
			assert.Equal(t, resolvedB, s.Cwd, `reloading with "/" must not reset the session's workspace`)
		}
	}
}
