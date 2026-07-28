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

// TestE2E_Wire_TerminalPassthrough pins that the `!` passthrough streams a command's output as a contenox.terminalOutput _meta session/update over the real ACP wire.
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

	// Output streams as a _meta update written before the run response, since
	// the flush interval is well under the run capture window.
	runResp, notes := client.call(extMethodTerminalRun, terminalRunParams{
		SessionID: string(sid),
		Command:   "echo hallo-welt",
	})
	require.Nil(t, runResp.Error, "terminal run must succeed")

	got := collectTerminalOutput(t, client, sid, notes, "hallo-welt", 5*time.Second)
	assert.Contains(t, got, "hallo-welt", "streamed terminal output must contain the command result")

	// A second `!` line must reuse the live subscription rather than repaint the
	// panel. The subscribe happens synchronously inside the run handler, so any
	// Reset it emitted would land in this call's notifications.
	runResp2, notes2 := client.call(extMethodTerminalRun, terminalRunParams{
		SessionID: string(sid),
		Command:   "echo zweite-zeile",
	})
	require.Nil(t, runResp2.Error, "second terminal run must succeed")
	assert.Zero(t, countTerminalResets(t, sid, notes2),
		"a second `!` line must reuse the live subscription, not re-deliver the whole scrollback")

	got2 := collectTerminalOutput(t, client, sid, notes2, "zweite-zeile", 5*time.Second)
	assert.Contains(t, got2, "zweite-zeile")
}

// countTerminalResets counts sid's contenox.terminalOutput notifications
// carrying the Reset flag.
func countTerminalResets(t *testing.T, sid libacp.SessionID, notes []libacp.Notification) int {
	t.Helper()
	n := 0
	for _, note := range notes {
		if note.Method != libacp.MethodSessionUpdate {
			continue
		}
		var sn libacp.SessionNotification
		if json.Unmarshal(note.Params, &sn) != nil {
			continue
		}
		if sn.SessionID != sid || sn.Update.SessionUpdate != TerminalOutputUpdateKind {
			continue
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(sn.Update.Meta, &meta) != nil {
			continue
		}
		var payload terminalOutputPayload
		if json.Unmarshal(meta[TerminalOutputMetaKey], &payload) != nil {
			continue
		}
		if payload.Reset {
			n++
		}
	}
	return n
}

// TestE2E_Wire_ExternalAgent_TerminalPassthrough pins that the `!` passthrough runs on contenox's own shell, not the downstream agent's, even when the session is bound to an external agent.
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

// collectTerminalOutput accumulates contenox.terminalOutput chunks for sid
// until want is seen or the deadline elapses.
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
