//go:build !windows

package acpsvc

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// externalTerminalWire wires a real production Transport to a wireClient over
// an NDJSON pipe, plus a stub agent opted into the terminal scenario via env.
func externalTerminalWire(t *testing.T, ctx context.Context, db libdb.DBManager, shells shellsession.Manager, env map[string]string) (*wireClient, string) {
	t.Helper()
	agentName := registerStubAgentInDB(t, db, "claude-stub-terminal", env)

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factory := New(Deps{
		Engine:        &enginesvc.Engine{},
		DB:            db,
		WorkspaceID:   "wire-ext-terminal",
		ShellSessions: shells,
	})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		return factory(c)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Error("connection did not shut down")
		}
	})
	return &wireClient{t: t, rw: clientSide}, agentName
}

func newTerminalShellManager(t *testing.T) shellsession.Manager {
	t.Helper()
	root := t.TempDir()
	// A Factory, not a bare root: shellsession enforces the workspace envelope
	// through vfs.ResolveSessionCwd.
	roots, err := vfs.NewFactory(root)
	require.NoError(t, err)
	mgr := shellsession.NewManager(shellsession.Config{
		CwdResolver: func(context.Context) string { return root },
		Workspace:   roots,
		IdleTimeout: time.Minute,
	})
	t.Cleanup(mgr.Shutdown)
	return mgr
}

// collectAgentChunk accumulates agent_message_chunk text for sid until a chunk
// containing want is seen (returned) or the deadline elapses.
func collectAgentChunk(t *testing.T, c *wireClient, sid libacp.SessionID, seed []libacp.Notification, want string, timeout time.Duration) string {
	t.Helper()
	scan := func(n libacp.Notification) (string, bool) {
		if n.Method != libacp.MethodSessionUpdate {
			return "", false
		}
		var note libacp.SessionNotification
		if json.Unmarshal(n.Params, &note) != nil {
			return "", false
		}
		if note.SessionID != sid || note.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			return "", false
		}
		if note.Update.Content == nil {
			return "", false
		}
		txt := note.Update.Content.Text
		return txt, strings.Contains(txt, want)
	}
	for _, n := range seed {
		if txt, ok := scan(n); ok {
			return txt
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
			if in.Kind == libacp.IncomingKindNotification {
				if txt, ok := scan(in.Notification); ok {
					return txt
				}
			}
		case <-deadline:
			t.Fatalf("did not observe an agent_message_chunk containing %q", want)
			return ""
		}
	}
}

// TestE2E_Wire_ExternalAgent_TerminalRoundTrip pins: a shell command's real
// output both returns downstream and streams to the upstream terminal panel.
func TestE2E_Wire_ExternalAgent_TerminalRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/wire-ext-terminal.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	shells := newTerminalShellManager(t)
	client, agentName := externalTerminalWire(t, ctx, db, shells, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

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
	require.Equal(t, agentName, parseAgentMeta(newResp.Meta))

	resp, notes := client.call(libacp.MethodSessionPrompt, libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("run a terminal command")},
	})
	require.Nil(t, resp.Error, "the external terminal prompt turn must complete")

	report := collectAgentChunk(t, client, sid, notes, "terminal-scenario", 8*time.Second)
	require.Contains(t, report, "termcap=true",
		"the downstream initialize must have advertised the terminal client capability")
	require.NotContains(t, report, "-error=", "no terminal/* lifecycle call may error: "+report)
	require.Contains(t, report, "exit=0", "the command exited cleanly through WaitForTerminalExit: "+report)
	require.Contains(t, report, "truncated=false", "the small output must not be truncated: "+report)
	require.Contains(t, report, "stub-terminal-42",
		"TerminalOutput must return the command's real output back to the downstream agent: "+report)

	got := collectTerminalOutput(t, client, sid, notes, "stub-terminal-42", 8*time.Second)
	assert.Contains(t, got, "stub-terminal-42",
		"the runtime shell's output must ALSO stream to the upstream terminal panel (contenox.terminalOutput)")
}

// TestE2E_Wire_ExternalAgent_TerminalPanelFiltered pins: the panel stream
// carries only the real output and a clean header, never the bridge's framing.
func TestE2E_Wire_ExternalAgent_TerminalPanelFiltered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/wire-ext-terminal-panel.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	shells := newTerminalShellManager(t)
	client, agentName := externalTerminalWire(t, ctx, db, shells, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

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

	resp, notes := client.call(libacp.MethodSessionPrompt, libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("run a terminal command")},
	})
	require.Nil(t, resp.Error, "the external terminal prompt turn must complete")

	// Panel forwarding is synchronous with the turn, so the whole panel stream
	// for this command is already in notes.
	require.Contains(t,
		collectTerminalOutput(t, client, sid, notes, "stub-terminal-42", 8*time.Second),
		"stub-terminal-42", "the command's real output must reach the terminal panel")
	panel := collectTerminalPanel(sid, notes)

	require.Contains(t, panel, "stub-terminal-42",
		"the command's real output must reach the terminal panel")
	require.Contains(t, panel, "$ echo stub-terminal-$((6*7)) | cat",
		"the panel shows a clean command header using the agent's requested command")
	require.NotContains(t, panel, "CTXS", "the START marker token must never reach the panel")
	require.NotContains(t, panel, "CTXE", "the END marker token must never reach the panel")
	require.NotContains(t, panel, "bash -c", "the bridge's bash -c wrapper must never reach the panel")
	require.NotContains(t, panel, "\x1b[2K", "the erase-line control bytes must never reach the panel")
}

// collectTerminalPanel accumulates, in order, every contenox.terminalOutput chunk
// for sid — the panel-bound stream as the upstream client received it.
func collectTerminalPanel(sid libacp.SessionID, notes []libacp.Notification) string {
	var acc strings.Builder
	for _, n := range notes {
		if n.Method != libacp.MethodSessionUpdate {
			continue
		}
		var note libacp.SessionNotification
		if json.Unmarshal(n.Params, &note) != nil {
			continue
		}
		if note.SessionID != sid || note.Update.SessionUpdate != TerminalOutputUpdateKind {
			continue
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal(note.Update.Meta, &meta) != nil {
			continue
		}
		var payload terminalOutputPayload
		if json.Unmarshal(meta[TerminalOutputMetaKey], &payload) != nil {
			continue
		}
		acc.WriteString(payload.Chunk)
	}
	return acc.String()
}

// TestE2E_Wire_ExternalAgent_TerminalKillReleaseLifecycle pins: killing a
// running command resolves WaitForTerminalExit promptly, not by blocking.
func TestE2E_Wire_ExternalAgent_TerminalKillReleaseLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/wire-ext-terminal-kill.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	shells := newTerminalShellManager(t)
	client, agentName := externalTerminalWire(t, ctx, db, shells, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

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

	// A 30s sleep is killed immediately; the turn finishing far under that.
	start := time.Now()
	resp, notes := client.call(libacp.MethodSessionPrompt, libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("terminal_kill please")},
	})
	require.Nil(t, resp.Error)
	require.Less(t, time.Since(start), 15*time.Second,
		"kill must interrupt the command; WaitForTerminalExit must not block for the command's natural duration")

	report := collectAgentChunk(t, client, sid, notes, "kill exit=", 8*time.Second)
	require.NotContains(t, report, "-error=", "the kill/wait/release lifecycle must be clean: "+report)
	require.Contains(t, report, "signal:SIGINT",
		"a killed command resolves with the SIGINT the shared-shell interrupt sends: "+report)
	require.NotContains(t, report, "should-not-appear",
		"the killed command's post-sleep output must never have run")
}

// TestE2E_Wire_ExternalAgent_TerminalCapabilityWithheldWhenNoShellManager pins:
// with no shell manager, initialize advertises no terminal capability.
func TestE2E_Wire_ExternalAgent_TerminalCapabilityWithheldWhenNoShellManager(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/wire-ext-terminal-nocap.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	// No ShellSessions manager: the runtime must not advertise Terminal downstream.
	client, agentName := externalTerminalWire(t, ctx, db, nil, map[string]string{"ACP_STUB_USE_TERMINAL": "1"})

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

	resp, notes := client.call(libacp.MethodSessionPrompt, libacp.PromptRequest{
		SessionID: sid,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("run a terminal command")},
	})
	require.Nil(t, resp.Error, "the turn still completes when the terminal capability is absent")

	report := collectAgentChunk(t, client, sid, notes, "terminal-scenario", 8*time.Second)
	require.Contains(t, report, "termcap=false",
		"without a shell manager the runtime must not advertise the terminal capability, and the agent must not attempt terminal/*")
}

// TestUnit_ComposeTerminalCommand_ShapeAndQuoting pins: safe quoting of
// command/args/env/cwd, and the END regex matching only the printed marker.
func TestUnit_ComposeTerminalCommand_ShapeAndQuoting(t *testing.T) {
	line := composeTerminalCommand(libacp.CreateTerminalRequest{
		Command: "echo",
		Args:    []string{"a b", "it's"},
		Env:     []libacp.EnvVariable{{Name: "K", Value: "v"}},
		Cwd:     "/tmp/x y",
	}, "CTXSnonce", "CTXEnonce")

	require.Contains(t, line, "( cd '/tmp/x y' && env 'K'='v' 'echo' 'a b' 'it'\\''s' )",
		"cwd/env/args must be single-quoted and run in a subshell so $? is the command's exit")
	require.Contains(t, line, "__ce=$?", "the exit code must be captured right after the subshell")
	require.Contains(t, line, `printf 'CTXSnonce%d`, "the START marker format embeds %d, not a literal digit")
	require.Contains(t, line, `printf '\nCTXEnonce %d`, "the END marker format embeds %d, not a literal digit")

	bt := &bridgeTerminal{
		startRe: startMarkerRegexp("CTXSnonce"),
		endRe:   endMarkerRegexp("CTXEnonce"),
	}
	require.Nil(t, bt.endRe.FindStringIndex(line),
		"the END regex must not match the echoed format string, only the printed marker")

	raw := "CTXSnonce0" + terminalEraseSeq + "hello\n" + "\nCTXEnonce 7" + terminalEraseSeq
	out, sawStart, sawEnd, code := bt.locate(raw)
	require.True(t, sawStart)
	require.True(t, sawEnd)
	require.Equal(t, "hello\n", out)
	require.NotNil(t, code)
	require.Equal(t, 7, *code)
}

// TestUnit_ComposeTerminalCommand_ShellLineVsExecvp pins: empty Args runs as a
// shell line via `bash -c`; non-empty Args is execvp-style, quoted per atom.
func TestUnit_ComposeTerminalCommand_ShellLineVsExecvp(t *testing.T) {
	shellLine := composeTerminalCommand(libacp.CreateTerminalRequest{
		Command: "git status -s | head",
		Env:     []libacp.EnvVariable{{Name: "CLAUDECODE", Value: "1"}},
	}, "CTXSnonce", "CTXEnonce")
	require.Contains(t, shellLine, "( env 'CLAUDECODE'='1' bash -c 'git status -s | head' )",
		"a no-args command is a shell line: run via bash -c with env applied to the whole line, pipe intact")
	require.Contains(t, shellLine, "__ce=$?",
		"the subshell wraps the bash -c invocation, so $? is the shell line's exit")

	require.Contains(t,
		composeTerminalCommand(libacp.CreateTerminalRequest{Command: "echo hello"}, "CTXSnonce", "CTXEnonce"),
		"( bash -c 'echo hello' )",
		"a no-args command must never be quoted as a single execvp atom ('echo hello')")

	execvp := composeTerminalCommand(libacp.CreateTerminalRequest{
		Command: "echo", Args: []string{"hello"},
	}, "CTXSnonce", "CTXEnonce")
	require.Contains(t, execvp, "( 'echo' 'hello' )",
		"a command WITH args is execvp-style: command and each arg quoted separately")
	require.NotContains(t, execvp, "bash -c",
		"the execvp path must not wrap in a shell")
}
