//go:build !windows

package acpsvc

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_Wire_TerminalPassthrough drives the `!` passthrough entrypoint over
// the real ACP transport: session/new, then the _contenox/terminal/run extension
// request, and asserts the command's output is streamed back as a
// contenox.terminalOutput _meta session/update — the same transport a foreign
// client would simply ignore.
func TestE2E_Wire_TerminalPassthrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dir := t.TempDir()
	db, err := libdb.NewSQLiteDBManager(ctx, dir+"/wire-term.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	mgr := newTerminalShellManager(t)

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factoryFn := New(Deps{
		Engine:        &enginesvc.Engine{},
		DB:            db,
		WorkspaceID:   "wire-term",
		ShellSessions: mgr,
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

	resp, _ = client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        "/",
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	sid := newResp.SessionID

	// The passthrough runs one line without an LLM turn; output streams as a
	// contenox.terminalOutput _meta update (written before the run response, since
	// the flush interval is well under the run capture window).
	runResp, notes := client.call(extMethodTerminalRun, terminalRunParams{
		SessionID: string(sid),
		Command:   "echo hallo-welt",
	})
	require.Nil(t, runResp.Error, "terminal run must succeed")

	got := collectTerminalOutput(t, client, sid, notes, "hallo-welt", 5*time.Second)
	assert.Contains(t, got, "hallo-welt", "streamed terminal output must contain the command result")
}

// TestE2E_Wire_ExternalAgent_TerminalPassthrough proves the `!` passthrough is
// agent-agnostic: it runs on the RUNTIME's own shell (contenox's ShellSessions,
// rooted at the session cwd), NOT the downstream agent's terminal, so it works
// identically on a session bound to an external agent. Driven over the real ACP
// transport: session/new carrying contenox.agent spawns the hermetic stub agent,
// then _contenox/terminal/run streams the runtime shell's output back as a
// contenox.terminalOutput _meta update — the downstream agent is never involved
// (and note NewSession's external branch never subscribes the terminal; the run
// entrypoint self-subscribes, which is exactly what this exercises).
func TestE2E_Wire_ExternalAgent_TerminalPassthrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	db, err := libdb.NewSQLiteDBManager(ctx, dir+"/wire-ext-term.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	agentName := registerStubAgentInDB(t, db, "claude-stub-term", nil)

	mgr := newTerminalShellManager(t)

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factoryFn := New(Deps{
		Engine:        &enginesvc.Engine{},
		DB:            db,
		WorkspaceID:   "wire-ext-term",
		ShellSessions: mgr,
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

	resp, _ = client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        "/",
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Nil(t, resp.Error)
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	sid := newResp.SessionID
	require.Equal(t, agentName, parseAgentMeta(newResp.Meta),
		"the session must be bound to the external agent")

	runResp, notes := client.call(extMethodTerminalRun, terminalRunParams{
		SessionID: string(sid),
		Command:   "echo hallo-extern",
	})
	require.Nil(t, runResp.Error, "terminal run must succeed on an external session")

	got := collectTerminalOutput(t, client, sid, notes, "hallo-extern", 5*time.Second)
	assert.Contains(t, got, "hallo-extern",
		"the runtime's own shell output must stream even when the session is bound to an external agent")
}

// collectTerminalOutput accumulates contenox.terminalOutput chunks for sid from
// the already-received notes and any further notifications until want is seen or
// the deadline elapses.
func collectTerminalOutput(t *testing.T, c *wireClient, sid libacp.SessionID, seed []libacp.Notification, want string, timeout time.Duration) string {
	t.Helper()
	var acc strings.Builder
	scan := func(n libacp.Notification) bool {
		if n.Method != libacp.MethodSessionUpdate {
			return false
		}
		var note libacp.SessionNotification
		if err := json.Unmarshal(n.Params, &note); err != nil {
			return false
		}
		if note.SessionID != sid || note.Update.SessionUpdate != TerminalOutputUpdateKind {
			return false
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(note.Update.Meta, &meta); err != nil {
			return false
		}
		var payload terminalOutputPayload
		if err := json.Unmarshal(meta[TerminalOutputMetaKey], &payload); err != nil {
			return false
		}
		acc.WriteString(payload.Chunk)
		return strings.Contains(acc.String(), want)
	}
	for _, n := range seed {
		if scan(n) {
			return acc.String()
		}
	}

	ch := make(chan libacp.Incoming)
	go func() {
		for {
			ch <- c.read()
		}
	}()
	deadline := time.After(timeout)
	for {
		select {
		case in := <-ch:
			if in.Kind == libacp.IncomingKindNotification && scan(in.Notification) {
				return acc.String()
			}
		case <-deadline:
			return acc.String()
		}
	}
}
