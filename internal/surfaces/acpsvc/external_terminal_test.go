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
	"github.com/contenox/beam/internal/services/shellsession"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// externalTerminalWire wires a real production Transport (with a shell manager
// rooted at root) to a wireClient over an NDJSON pipe, plus a registered stub
// agent opted into the terminal scenario via the given env. It returns the client
// and the registered agent name; teardown is via t.Cleanup.
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
	// Give the manager a real one-root allowlist rather than a bare default
	// root: shellsession enforces the workspace envelope through
	// vfs.ResolveSessionCwd, so a Factory is what production hands it.
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

// collectAgentChunk accumulates agent_message_chunk text for sid from the seed
// notifications and any further ones until a chunk whose text contains want is
// seen (returned) or the deadline elapses.
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

// TestE2E_Wire_ExternalAgent_TerminalRoundTrip is the keystone: a downstream
// external agent (the hermetic stub, opted into the terminal scenario) runs a
// shell command through the RUNTIME's terminals — CreateTerminal + WaitForTerminalExit
// + TerminalOutput + ReleaseTerminal — and (a) receives the command's real output
// back, with a clean exit code, while (b) that output ALSO streams to the upstream
// client as contenox.terminalOutput panel updates. Both are asserted over the raw
// wire against the real Transport.
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

	// (a) The downstream agent's report proves it received the runtime shell's
	// output with a clean exit — no lifecycle call errored.
	report := collectAgentChunk(t, client, sid, notes, "terminal-scenario", 8*time.Second)
	require.Contains(t, report, "termcap=true",
		"the downstream initialize must have advertised the terminal client capability")
	require.NotContains(t, report, "-error=", "no terminal/* lifecycle call may error: "+report)
	require.Contains(t, report, "exit=0", "the command exited cleanly through WaitForTerminalExit: "+report)
	require.Contains(t, report, "truncated=false", "the small output must not be truncated: "+report)
	require.Contains(t, report, "stub-terminal-42",
		"TerminalOutput must return the command's real output back to the downstream agent: "+report)

	// (b) That same output also reached the upstream client's terminal panel path.
	got := collectTerminalOutput(t, client, sid, notes, "stub-terminal-42", 8*time.Second)
	assert.Contains(t, got, "stub-terminal-42",
		"the runtime shell's output must ALSO stream to the upstream terminal panel (contenox.terminalOutput)")
}

// TestE2E_Wire_ExternalAgent_TerminalPanelFiltered pins the fix for the live
// defect: when a downstream agent runs a shell command, beam's terminal panel used
// to show the bridge's INTERNAL scaffolding as raw text — the wrapped command line
// (`env … bash -c …`) and the CTXS/CTXE framing markers with their `\033[2K\r`
// erase sequences — because the panel is an append-only log view that does not
// process cursor controls, so the erase-wrapped markers were never hidden. The
// panel-bound stream (contenox.terminalOutput) must now carry ONLY the command's
// real output, preceded by a clean `$ <command>` header in the AGENT's requested
// form — never the wrapper, the markers, or the erase bytes.
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

	// The panel forwarding is synchronous with the turn: the bridge writes each
	// terminal-output chunk to the wire before WaitForTerminalExit resolves, hence
	// before the prompt response — so the whole panel-bound stream for this command
	// is already in the notifications collected during the call. Wait for the marker
	// (drains if a chunk trails), then assert the FULL panel stream from the seed so
	// even a trailing END-marker leak would be caught.
	require.Contains(t,
		collectTerminalOutput(t, client, sid, notes, "stub-terminal-42", 8*time.Second),
		"stub-terminal-42", "the command's real output must reach the terminal panel")
	panel := collectTerminalPanel(sid, notes)

	// (1) The real command output reached the panel — proof output still streams.
	require.Contains(t, panel, "stub-terminal-42",
		"the command's real output must reach the terminal panel")
	// (2) A clean `$ <command>` header, in the AGENT's requested form — the raw
	// command line the agent asked for, NOT the bridge's bash -c wrapper.
	require.Contains(t, panel, "$ echo stub-terminal-$((6*7)) | cat",
		"the panel shows a clean command header using the agent's requested command")
	// (3) NONE of the bridge's internal framing leaks to the panel.
	require.NotContains(t, panel, "CTXS", "the START marker token must never reach the panel")
	require.NotContains(t, panel, "CTXE", "the END marker token must never reach the panel")
	require.NotContains(t, panel, "bash -c", "the bridge's bash -c wrapper must never reach the panel")
	require.NotContains(t, panel, "\x1b[2K", "the erase-line control bytes must never reach the panel")
}

// collectTerminalPanel accumulates, in order, every contenox.terminalOutput chunk
// for sid found in the given notifications — the panel-bound stream as the upstream
// client received it — so a test can assert on both the presence of real output and
// the ABSENCE of the bridge's internal framing.
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

// TestE2E_Wire_ExternalAgent_TerminalKillReleaseLifecycle drives the kill path: a
// long-running command is started, killed, and WaitForTerminalExit resolves
// promptly with a signal (rather than blocking for the command's full duration),
// then released cleanly.
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

	// A 30s sleep is started and immediately killed; the whole turn must finish far
	// under that, proving the kill (Ctrl-C) actually interrupted the command and
	// WaitForTerminalExit resolved.
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

// TestE2E_Wire_ExternalAgent_TerminalCapabilityWithheldWhenNoShellManager pins the
// declined/absent-capability path: with NO shell manager on the server, the
// downstream initialize advertises no terminal capability, so a terminal-using
// agent observes termcap=false and skips the round trip — the turn still completes
// normally, unaffected.
func TestE2E_Wire_ExternalAgent_TerminalCapabilityWithheldWhenNoShellManager(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, t.TempDir()+"/wire-ext-terminal-nocap.db", runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	// No ShellSessions manager: the runtime must NOT advertise Terminal to the
	// downstream, and the terminal-using stub must degrade gracefully.
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

// TestUnit_ComposeTerminalCommand_ShapeAndQuoting pins the wrapped shell line the
// bridge submits: START/END markers wrapping the command in a subshell with the
// exit code captured, and safe quoting of command/args/env/cwd. The markers embed
// a `%d` so the format string as it appears in the echoed input line never matches
// the OUTPUT regex — only the printed marker (a literal digit) does.
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

	// The OUTPUT regex the bridge uses to find the printed END marker must NOT match
	// the format string in the echoed input line (which carries %d, not a digit).
	bt := &bridgeTerminal{
		startRe: startMarkerRegexp("CTXSnonce"),
		endRe:   endMarkerRegexp("CTXEnonce"),
	}
	require.Nil(t, bt.endRe.FindStringIndex(line),
		"the END regex must not match the echoed format string, only the printed marker")

	// A synthesized scrollback with the PRINTED markers must slice cleanly to the
	// command output and recover the exit code.
	raw := "CTXSnonce0" + terminalEraseSeq + "hello\n" + "\nCTXEnonce 7" + terminalEraseSeq
	out, sawStart, sawEnd, code := bt.locate(raw)
	require.True(t, sawStart)
	require.True(t, sawEnd)
	// The command's own trailing newline is preserved; only the newline the END
	// printf injects ahead of its marker is stripped.
	require.Equal(t, "hello\n", out)
	require.NotNil(t, code)
	require.Equal(t, 7, *code)
}

// TestUnit_ComposeTerminalCommand_ShellLineVsExecvp pins the two request shapes:
// an EMPTY-args command is a full shell command line run via `bash -c` (so a pipe
// survives and env applies to the whole line), while a NON-empty-args command is
// execvp-style with each atom quoted separately and NO shell. This is the fix for
// the live bug where claude-code-acp's `command:"echo hello"` (no args) was quoted
// as the single atom `'echo hello'` and failed with exit 127.
func TestUnit_ComposeTerminalCommand_ShellLineVsExecvp(t *testing.T) {
	// No args, with a pipe and env: the full line goes to bash -c, env in front.
	shellLine := composeTerminalCommand(libacp.CreateTerminalRequest{
		Command: "git status -s | head",
		Env:     []libacp.EnvVariable{{Name: "CLAUDECODE", Value: "1"}},
	}, "CTXSnonce", "CTXEnonce")
	require.Contains(t, shellLine, "( env 'CLAUDECODE'='1' bash -c 'git status -s | head' )",
		"a no-args command is a shell line: run via bash -c with env applied to the whole line, pipe intact")
	require.Contains(t, shellLine, "__ce=$?",
		"the subshell wraps the bash -c invocation, so $? is the shell line's exit")

	// No args, no env: bare bash -c.
	require.Contains(t,
		composeTerminalCommand(libacp.CreateTerminalRequest{Command: "echo hello"}, "CTXSnonce", "CTXEnonce"),
		"( bash -c 'echo hello' )",
		"a no-args command must never be quoted as a single execvp atom ('echo hello')")

	// Args present: execvp-style, quoted per atom, no shell.
	execvp := composeTerminalCommand(libacp.CreateTerminalRequest{
		Command: "echo", Args: []string{"hello"},
	}, "CTXSnonce", "CTXEnonce")
	require.Contains(t, execvp, "( 'echo' 'hello' )",
		"a command WITH args is execvp-style: command and each arg quoted separately")
	require.NotContains(t, execvp, "bash -c",
		"the execvp path must not wrap in a shell")
}
