package acpexec_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libacp/acpexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests validate libacp's client-side wire dispatch against testy, the
// Rust reference SDK's deterministic ACP test agent, over a real subprocess
// (acpexec.Spawn). Opt-in: every test skips unless the caller points at a
// local testy build via env var (see `make acp-client-e2e`).
//
// testy has one known bug relevant here: initialize echoes back whatever
// protocolVersion it is sent instead of negotiating, so these tests always
// send libacp.ProtocolVersion and never exercise negotiation against it.
const (
	acpTestyBinEnv   = "ACP_TESTY_BIN"
	acpMcpEchoBinEnv = "ACP_MCP_ECHO_BIN"
)

// fakeTerminal is one CreateTerminal call's real, running child process: the
// e2e Client actually executes the requested command rather than faking
// output, so the terminal/* family has real process state to report.
type fakeTerminal struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	output   bytes.Buffer
	done     chan struct{}
	exitCode *int
	signal   *string
}

type termWriter struct{ t *fakeTerminal }

func (w termWriter) Write(p []byte) (int, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	return w.t.output.Write(p)
}

// e2eClient is the libacp.Client this file's tests wire a real
// ClientSideConnection to: it records every agent-initiated call, serves
// fs/read_text_file and fs/write_text_file from an in-memory map, answers
// session/request_permission with the first offered option (unless held open,
// see permissionEntered), and serves terminal/* against real child processes.
type e2eClient struct {
	libacp.UnimplementedClient

	mu           sync.Mutex
	updates      []libacp.SessionNotification
	permCalls    []libacp.RequestPermissionRequest
	readCalls    []libacp.ReadTextFileRequest
	writeCalls   []libacp.WriteTextFileRequest
	createCalls  []libacp.CreateTerminalRequest
	outputCalls  []libacp.TerminalOutputRequest
	waitCalls    []libacp.WaitForTerminalExitRequest
	killCalls    []libacp.KillTerminalRequest
	releaseCalls []libacp.ReleaseTerminalRequest

	fs        map[string]string
	terminals map[string]*fakeTerminal
	termSeq   int

	// permissionEntered, when set before a call begins, is closed the moment
	// RequestPermission is invoked; the call then blocks on ctx instead of
	// answering, so a test can exercise CancelPrompt while it is pending.
	permissionEntered chan struct{}
}

func (c *e2eClient) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, n)
	c.mu.Unlock()
	return nil
}

func (c *e2eClient) RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permCalls = append(c.permCalls, req)
	entered := c.permissionEntered
	c.mu.Unlock()

	if entered != nil {
		close(entered)
		<-ctx.Done()
		return libacp.RequestPermissionResponse{}, ctx.Err()
	}

	if len(req.Options) == 0 {
		return libacp.RequestPermissionResponse{}, libacp.InvalidParams("no permission options offered")
	}
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{
			Outcome:  libacp.PermissionOutcomeSelected,
			OptionID: req.Options[0].OptionID,
		},
	}, nil
}

func (c *e2eClient) WriteTextFile(_ context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error) {
	c.mu.Lock()
	c.writeCalls = append(c.writeCalls, req)
	if c.fs == nil {
		c.fs = make(map[string]string)
	}
	c.fs[req.Path] = req.Content
	c.mu.Unlock()
	return libacp.WriteTextFileResponse{}, nil
}

func (c *e2eClient) ReadTextFile(_ context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	c.mu.Lock()
	c.readCalls = append(c.readCalls, req)
	content, ok := c.fs[req.Path]
	c.mu.Unlock()
	if !ok {
		return libacp.ReadTextFileResponse{}, libacp.NewError(libacp.ErrResourceNotFound, "no such file: "+req.Path)
	}

	lines := strings.Split(content, "\n")
	start := 0
	if req.Line != nil && *req.Line > 1 {
		start = *req.Line - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if req.Limit != nil && start+*req.Limit < end {
		end = start + *req.Limit
	}
	return libacp.ReadTextFileResponse{Content: strings.Join(lines[start:end], "\n")}, nil
}

func (c *e2eClient) CreateTerminal(_ context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	c.mu.Lock()
	c.createCalls = append(c.createCalls, req)
	c.termSeq++
	id := fmt.Sprintf("e2e-term-%d", c.termSeq)
	c.mu.Unlock()

	cmd := exec.Command(req.Command, req.Args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	for _, e := range req.Env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}
	term := &fakeTerminal{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = termWriter{term}
	cmd.Stderr = termWriter{term}

	if err := cmd.Start(); err != nil {
		return libacp.CreateTerminalResponse{}, libacp.InternalError(err.Error())
	}

	go func() {
		waitErr := cmd.Wait()
		term.mu.Lock()
		switch {
		case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0:
			code := cmd.ProcessState.ExitCode()
			term.exitCode = &code
		case waitErr != nil:
			msg := waitErr.Error()
			term.signal = &msg
		default:
			zero := 0
			term.exitCode = &zero
		}
		term.mu.Unlock()
		close(term.done)
	}()

	c.mu.Lock()
	if c.terminals == nil {
		c.terminals = make(map[string]*fakeTerminal)
	}
	c.terminals[id] = term
	c.mu.Unlock()

	return libacp.CreateTerminalResponse{TerminalID: id}, nil
}

func (c *e2eClient) TerminalOutput(_ context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	c.mu.Lock()
	c.outputCalls = append(c.outputCalls, req)
	term := c.terminals[req.TerminalID]
	c.mu.Unlock()
	if term == nil {
		return libacp.TerminalOutputResponse{}, libacp.InvalidParams("unknown terminal " + req.TerminalID)
	}

	term.mu.Lock()
	defer term.mu.Unlock()
	resp := libacp.TerminalOutputResponse{Output: term.output.String()}
	select {
	case <-term.done:
		resp.ExitStatus = &libacp.TerminalExitStatus{ExitCode: term.exitCode, Signal: term.signal}
	default:
	}
	return resp, nil
}

func (c *e2eClient) WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	c.mu.Lock()
	c.waitCalls = append(c.waitCalls, req)
	term := c.terminals[req.TerminalID]
	c.mu.Unlock()
	if term == nil {
		return libacp.WaitForTerminalExitResponse{}, libacp.InvalidParams("unknown terminal " + req.TerminalID)
	}

	select {
	case <-term.done:
	case <-ctx.Done():
		return libacp.WaitForTerminalExitResponse{}, ctx.Err()
	}
	term.mu.Lock()
	defer term.mu.Unlock()
	return libacp.WaitForTerminalExitResponse{ExitCode: term.exitCode, Signal: term.signal}, nil
}

func (c *e2eClient) KillTerminal(_ context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	c.mu.Lock()
	c.killCalls = append(c.killCalls, req)
	term := c.terminals[req.TerminalID]
	c.mu.Unlock()
	if term != nil && term.cmd.Process != nil {
		_ = term.cmd.Process.Kill()
	}
	return libacp.KillTerminalResponse{}, nil
}

func (c *e2eClient) ReleaseTerminal(_ context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	c.mu.Lock()
	c.releaseCalls = append(c.releaseCalls, req)
	delete(c.terminals, req.TerminalID)
	c.mu.Unlock()
	return libacp.ReleaseTerminalResponse{}, nil
}

func (c *e2eClient) snapshotUpdates() []libacp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.SessionNotification(nil), c.updates...)
}

func (c *e2eClient) snapshotPermCalls() []libacp.RequestPermissionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.RequestPermissionRequest(nil), c.permCalls...)
}

func (c *e2eClient) snapshotReadCalls() []libacp.ReadTextFileRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.ReadTextFileRequest(nil), c.readCalls...)
}

func (c *e2eClient) snapshotWriteCalls() []libacp.WriteTextFileRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.WriteTextFileRequest(nil), c.writeCalls...)
}

func (c *e2eClient) snapshotCreateCalls() []libacp.CreateTerminalRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.CreateTerminalRequest(nil), c.createCalls...)
}

func (c *e2eClient) snapshotOutputCalls() []libacp.TerminalOutputRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.TerminalOutputRequest(nil), c.outputCalls...)
}

func (c *e2eClient) snapshotWaitCalls() []libacp.WaitForTerminalExitRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.WaitForTerminalExitRequest(nil), c.waitCalls...)
}

func (c *e2eClient) snapshotKillCalls() []libacp.KillTerminalRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.KillTerminalRequest(nil), c.killCalls...)
}

func (c *e2eClient) snapshotReleaseCalls() []libacp.ReleaseTerminalRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]libacp.ReleaseTerminalRequest(nil), c.releaseCalls...)
}

// testyHarness spawns a fresh testy process per test (avoiding any shared,
// cross-test session state) and wires a real ClientSideConnection to it over
// acpexec.Spawn.
type testyHarness struct {
	t      *testing.T
	conn   *libacp.ClientSideConnection
	client *e2eClient
	proc   *acpexec.Process
	stderr *acpexec.LockedBuffer
	runErr chan error
	ctx    context.Context
}

func newTestyHarness(t *testing.T) *testyHarness {
	t.Helper()
	testyBin := os.Getenv(acpTestyBinEnv)
	if testyBin == "" {
		t.Skipf("skipping: set %s to a built testy binary to run (see `make acp-client-e2e`)", acpTestyBinEnv)
	}
	if _, err := os.Stat(testyBin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", acpTestyBinEnv, testyBin, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// testy is a persistent process that never exits on stdin EOF, so every
	// cleanup takes acpexec.Close's kill path; a short grace avoids paying
	// the default 5s on every test.
	var stderr acpexec.LockedBuffer
	proc, err := acpexec.Spawn(ctx, exec.Command(testyBin), acpexec.WithStderr(&stderr), acpexec.WithKillGrace(500*time.Millisecond))
	if err != nil {
		t.Fatalf("spawn testy (%s): %v", testyBin, err)
	}

	client := &e2eClient{}
	conn := libacp.NewClientSideConnection(proc, func(*libacp.ClientSideConnection) libacp.Client { return client })

	runErr := make(chan error, 1)
	go func() { runErr <- conn.Run(ctx) }()

	h := &testyHarness{t: t, conn: conn, client: client, proc: proc, stderr: &stderr, runErr: runErr, ctx: ctx}
	t.Cleanup(h.cleanup)
	return h
}

func (h *testyHarness) cleanup() {
	_ = h.proc.Close()
	select {
	case <-h.runErr:
	case <-time.After(5 * time.Second):
		h.t.Errorf("ClientSideConnection.Run did not exit after the testy process was closed; testy stderr:\n%s", h.stderr.String())
	}
}

// fatalf fails the test, always including testy's captured stderr so a
// protocol mismatch is diagnosable without rerunning by hand.
func (h *testyHarness) fatalf(format string, args ...any) {
	h.t.Helper()
	h.t.Fatalf("%s\ntesty stderr:\n%s", fmt.Sprintf(format, args...), h.stderr.String())
}

func (h *testyHarness) initialize(t *testing.T) libacp.InitializeResponse {
	t.Helper()
	resp, err := h.conn.Initialize(h.ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{
			FS:       libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
		ClientInfo: &libacp.Implementation{Name: "libacp-e2e-client", Version: "test"},
	})
	if err != nil {
		h.fatalf("initialize: %v", err)
	}
	return resp
}

// newSession creates a session rooted at a fresh temp dir with one additional
// directory activated, exercising testy's advertised
// sessionCapabilities.additionalDirectories.
func (h *testyHarness) newSession(t *testing.T, mcpServers []libacp.McpServer) (sessionID libacp.SessionID, cwd string, extraDir string) {
	t.Helper()
	cwd = t.TempDir()
	extraDir = filepath.Join(cwd, "extra")
	require.NoError(t, os.MkdirAll(extraDir, 0o755))
	if mcpServers == nil {
		mcpServers = []libacp.McpServer{}
	}
	resp, err := h.conn.NewSession(h.ctx, libacp.NewSessionRequest{
		Cwd:                   cwd,
		AdditionalDirectories: []string{extraDir},
		McpServers:            mcpServers,
	})
	if err != nil {
		h.fatalf("session/new: %v", err)
	}
	return resp.SessionID, cwd, extraDir
}

// testyPrompt JSON-serializes v (a TestyCommand-shaped value): testy's
// prompt text is a JSON command, per testy.rs's TestyCommand.
func testyPrompt(v any) []libacp.ContentBlock {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return []libacp.ContentBlock{libacp.NewTextContent(string(raw))}
}

func TestTesty_InitializeHandshake(t *testing.T) {
	h := newTestyHarness(t)
	resp := h.initialize(t)

	assert.Equal(t, libacp.ProtocolVersion, resp.ProtocolVersion)
	// testy never sends agentInfo on the wire (optional per spec).
	assert.Nil(t, resp.AgentInfo)

	caps := resp.AgentCapabilities
	assert.True(t, caps.LoadSession)
	assert.True(t, caps.PromptCapabilities.Image)
	assert.True(t, caps.PromptCapabilities.Audio)
	assert.True(t, caps.PromptCapabilities.EmbeddedContext)
	assert.True(t, caps.McpCapabilities.HTTP)
	assert.NotNil(t, caps.SessionCapabilities.List)
	assert.NotNil(t, caps.SessionCapabilities.Delete)
	assert.NotNil(t, caps.SessionCapabilities.Resume)
	assert.NotNil(t, caps.SessionCapabilities.Close)
	assert.NotNil(t, caps.SessionCapabilities.AdditionalDirectories)
	assert.NotNil(t, caps.Auth.Logout)

	require.Len(t, resp.AuthMethods, 1)
	assert.Equal(t, "testy-agent-auth", resp.AuthMethods[0].ID)
	assert.Equal(t, "", resp.AuthMethods[0].Type, "testy's only auth method is the stable, agent-handled kind, which serializes with no type discriminator")
}

func TestTesty_AuthenticateThenLogout(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)

	_, err := h.conn.Authenticate(h.ctx, libacp.AuthenticateRequest{MethodID: "testy-agent-auth"})
	require.NoError(t, err)

	_, err = h.conn.Logout(h.ctx, libacp.LogoutRequest{})
	require.NoError(t, err)
}

func TestTesty_NewSessionPromptGreet(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, nil)

	resp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt:    testyPrompt(map[string]any{"command": "greet"}),
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	updates := h.client.snapshotUpdates()
	require.NotEmpty(t, updates)
	last := updates[len(updates)-1].Update
	require.Equal(t, libacp.SessionUpdateAgentMessageChunk, last.SessionUpdate)
	require.NotNil(t, last.Content)
	assert.Equal(t, "Hello, world!", last.Content.Text)
}

func TestTesty_EchoRoundTrip(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, nil)

	const message = "the quick brown fox jumps over the lazy dog"
	resp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt:    testyPrompt(map[string]any{"command": "echo", "message": message}),
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	updates := h.client.snapshotUpdates()
	require.Len(t, updates, 1)
	assert.Equal(t, libacp.SessionUpdateAgentMessageChunk, updates[0].Update.SessionUpdate)
	require.NotNil(t, updates[0].Update.Content)
	assert.Equal(t, message, updates[0].Update.Content.Text)
}

// TestTesty_RunScenarioSessionUpdates pins that every stable session/update
// variant testy's session_updates scenario emits arrives and unmarshals
// correctly, by exact count per kind — a malformed notification is silently
// dropped on the wire, so "no error" alone would not catch a parse regression.
func TestTesty_RunScenarioSessionUpdates(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, nil)

	resp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt:    testyPrompt(map[string]any{"command": "run_scenario", "scenario": "session_updates"}),
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	updates := h.client.snapshotUpdates()
	counts := map[libacp.SessionUpdateKind]int{}
	byKind := map[libacp.SessionUpdateKind][]libacp.SessionUpdate{}
	for _, u := range updates {
		counts[u.Update.SessionUpdate]++
		byKind[u.Update.SessionUpdate] = append(byKind[u.Update.SessionUpdate], u.Update)
	}

	assert.Equal(t, 1, counts[libacp.SessionUpdateSessionInfo], "session_info_update")
	assert.Equal(t, 1, counts[libacp.SessionUpdateCurrentMode], "current_mode_update")
	assert.Equal(t, 1, counts[libacp.SessionUpdateConfigOption], "config_option_update")
	assert.Equal(t, 1, counts[libacp.SessionUpdateAvailableCommands], "available_commands_update")
	assert.Equal(t, 1, counts[libacp.SessionUpdateUsageUpdate], "usage_update")
	assert.Equal(t, 1, counts[libacp.SessionUpdateUserMessageChunk], "user_message_chunk")
	assert.Equal(t, 1, counts[libacp.SessionUpdateAgentThoughtChunk], "agent_thought_chunk")
	// agent_message_chunk: text, image, audio, resource_link, and embedded
	// resource content blocks, plus the scenario's own final report chunk.
	assert.Equal(t, 6, counts[libacp.SessionUpdateAgentMessageChunk], "agent_message_chunk")
	assert.Equal(t, 2, counts[libacp.SessionUpdateToolCall], "tool_call")
	assert.Equal(t, 3, counts[libacp.SessionUpdateToolCallUpdate], "tool_call_update")
	assert.Equal(t, 1, counts[libacp.SessionUpdatePlan], "plan")

	if t.Failed() {
		return
	}

	sessionInfo := byKind[libacp.SessionUpdateSessionInfo][0]
	assert.Equal(t, "Testy deterministic session", sessionInfo.Title)
	assert.Equal(t, "2026-01-01T00:00:00Z", sessionInfo.UpdatedAt)

	mode := byKind[libacp.SessionUpdateCurrentMode][0]
	assert.Equal(t, "chat", mode.CurrentModeID)

	configOpt := byKind[libacp.SessionUpdateConfigOption][0]
	require.Len(t, configOpt.ConfigOptions, 1)
	assert.Equal(t, "verbosity", configOpt.ConfigOptions[0].ID)
	assert.Equal(t, libacp.SessionConfigOptionTypeSelect, configOpt.ConfigOptions[0].Type)
	assert.Equal(t, "normal", configOpt.ConfigOptions[0].CurrentValue)
	assert.Len(t, configOpt.ConfigOptions[0].Options.AllValues(), 3)

	avail := byKind[libacp.SessionUpdateAvailableCommands][0]
	var names []string
	for _, c := range avail.AvailableCommands {
		names = append(names, c.Name)
	}
	assert.Contains(t, names, "full")
	assert.Contains(t, names, "callbacks")

	usage := byKind[libacp.SessionUpdateUsageUpdate][0]
	assert.Equal(t, 128, usage.Used)
	assert.Equal(t, 4096, usage.Size)
	require.NotNil(t, usage.Cost)
	assert.Equal(t, "USD", usage.Cost.Currency)

	userMsg := byKind[libacp.SessionUpdateUserMessageChunk][0]
	require.NotNil(t, userMsg.Content)
	assert.Contains(t, userMsg.Content.Text, "prompt content block")

	thought := byKind[libacp.SessionUpdateAgentThoughtChunk][0]
	require.NotNil(t, thought.Content)
	assert.Equal(t, "thinking deterministically", thought.Content.Text)

	var contentTypes []string
	var sawFinalReport bool
	for _, msg := range byKind[libacp.SessionUpdateAgentMessageChunk] {
		require.NotNil(t, msg.Content)
		contentTypes = append(contentTypes, msg.Content.Type)
		switch libacp.ContentKind(msg.Content.Type) {
		case libacp.ContentKindText:
			if strings.Contains(msg.Content.Text, "session_updates: sent") {
				sawFinalReport = true
			}
		case libacp.ContentKindImage:
			assert.Equal(t, "image/png", msg.Content.MimeType)
			assert.Equal(t, "file:///tmp/testy-pixel.png", msg.Content.URI)
			assert.NotEmpty(t, msg.Content.Data)
		case libacp.ContentKindAudio:
			assert.Equal(t, "audio/wav", msg.Content.MimeType)
			assert.NotEmpty(t, msg.Content.Data)
		case libacp.ContentKindResourceLink:
			assert.Equal(t, "file:///tmp/testy-reference.txt", msg.Content.URI)
			assert.Equal(t, "Testy reference", msg.Content.Name)
			assert.Equal(t, "text/plain", msg.Content.MimeType)
		case libacp.ContentKindResource:
			require.NotNil(t, msg.Content.Resource)
			assert.Equal(t, "embedded resource text from testy", msg.Content.Resource.Text)
			assert.Equal(t, "file:///tmp/testy-embedded.txt", msg.Content.Resource.URI)
		}
	}
	assert.ElementsMatch(t, []string{"text", "image", "audio", "resource_link", "resource", "text"}, contentTypes)
	assert.True(t, sawFinalReport, "the scenario's own final report chunk must be among the agent_message_chunk updates")

	for _, tc := range byKind[libacp.SessionUpdateToolCall] {
		// testy omits "status" on a freshly created tool_call (relying on the
		// spec's implicit "pending" default), so the parsed Status is "".
		assert.Equal(t, libacp.ToolCallStatus(""), tc.Status)
		assert.NotEmpty(t, tc.ToolCallID)
	}

	var sawDiff bool
	for _, tcu := range byKind[libacp.SessionUpdateToolCallUpdate] {
		if tcu.Status != libacp.ToolCallStatusCompleted {
			continue
		}
		for _, c := range tcu.ToolContent {
			if c.Type == libacp.ToolCallContentDiff {
				sawDiff = true
				assert.Equal(t, "/tmp/testy-output.txt", c.Path)
				assert.Equal(t, "before\n", c.OldText)
				assert.Equal(t, "after\n", c.NewText)
			}
		}
	}
	assert.True(t, sawDiff, "the edit tool call's completed update must carry a diff content block")

	plan := byKind[libacp.SessionUpdatePlan][0]
	require.Len(t, plan.Entries, 3)
	assert.Equal(t, libacp.PlanStatusCompleted, plan.Entries[0].Status)
}

func TestTesty_RunScenarioToolCalls(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, nil)

	resp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt:    testyPrompt(map[string]any{"command": "run_scenario", "scenario": "tool_calls"}),
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, resp.StopReason)

	var toolCalls, toolCallUpdates, plans int
	for _, u := range h.client.snapshotUpdates() {
		switch u.Update.SessionUpdate {
		case libacp.SessionUpdateToolCall:
			toolCalls++
		case libacp.SessionUpdateToolCallUpdate:
			toolCallUpdates++
		case libacp.SessionUpdatePlan:
			plans++
		}
	}
	assert.Equal(t, 2, toolCalls)
	assert.Equal(t, 3, toolCallUpdates)
	assert.Equal(t, 1, plans)
}

// TestTesty_RunScenarioCallbacks pins that testy's callbacks scenario drives
// every stable agent-to-client request this Client serves (permission, fs
// read/write, terminal/*). The prebuilt binary's "unstable" feature then goes
// on to attempt elicitation/create, which this client never advertises, so
// the turn ends in an expected InvalidParams error rather than cleanly; the
// test judges the calls actually observed, not the turn's outcome.
func TestTesty_RunScenarioCallbacks(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, nil)

	_, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt:    testyPrompt(map[string]any{"command": "run_scenario", "scenario": "callbacks"}),
	})
	if err != nil {
		var rpcErr *libacp.Error
		require.ErrorAs(t, err, &rpcErr, "a testy scenario failure must be a wire-level RPC error, not a transport problem")
		assert.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
		t.Logf("run_scenario callbacks ended in testy's expected unstable-elicitation error: %v", rpcErr)
	}

	permCalls := h.client.snapshotPermCalls()
	require.Len(t, permCalls, 1)
	assert.Equal(t, sessID, permCalls[0].SessionID)
	assert.Equal(t, "Permission request", permCalls[0].ToolCall.Title)
	assert.Equal(t, libacp.ToolKindExecute, permCalls[0].ToolCall.Kind)
	require.Len(t, permCalls[0].Options, 2)
	assert.Equal(t, "allow_once", permCalls[0].Options[0].OptionID)
	assert.Equal(t, "reject_once", permCalls[0].Options[1].OptionID)

	writeCalls := h.client.snapshotWriteCalls()
	require.Len(t, writeCalls, 1)
	assert.Equal(t, sessID, writeCalls[0].SessionID)
	assert.Equal(t, "/tmp/testy-write.txt", writeCalls[0].Path)
	assert.Equal(t, "written by testy\n", writeCalls[0].Content)

	readCalls := h.client.snapshotReadCalls()
	require.Len(t, readCalls, 1)
	assert.Equal(t, "/tmp/testy-write.txt", readCalls[0].Path)
	require.NotNil(t, readCalls[0].Line)
	assert.Equal(t, 1, *readCalls[0].Line)
	require.NotNil(t, readCalls[0].Limit)
	assert.Equal(t, 20, *readCalls[0].Limit)

	createCalls := h.client.snapshotCreateCalls()
	require.Len(t, createCalls, 1)
	assert.Equal(t, "printf", createCalls[0].Command)
	assert.Equal(t, []string{"testy terminal\n"}, createCalls[0].Args)
	assert.Equal(t, "/tmp", createCalls[0].Cwd)

	assert.Len(t, h.client.snapshotOutputCalls(), 1)
	assert.Len(t, h.client.snapshotWaitCalls(), 1)
	assert.Len(t, h.client.snapshotKillCalls(), 1)
	assert.Len(t, h.client.snapshotReleaseCalls(), 1)
}

// TestTesty_CancelPromptMidScenario pins that CancelPrompt against a real,
// independent agent process resolves the turn with stopReason "cancelled"
// while a callback (session/request_permission) is genuinely pending.
func TestTesty_CancelPromptMidScenario(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, nil)

	entered := make(chan struct{})
	h.client.mu.Lock()
	h.client.permissionEntered = entered
	h.client.mu.Unlock()

	type result struct {
		resp libacp.PromptResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
			SessionID: sessID,
			Prompt:    testyPrompt(map[string]any{"command": "run_scenario", "scenario": "callbacks"}),
		})
		done <- result{resp, err}
	}()

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		h.fatalf("testy never asked for permission")
	}

	require.NoError(t, h.conn.CancelPrompt(sessID))

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, libacp.StopReasonCancelled, r.resp.StopReason)
	case <-time.After(15 * time.Second):
		h.fatalf("Prompt did not resolve after CancelPrompt")
	}
}

// TestTesty_McpPassDown pins session/new's MCP server pass-through: testy's
// list_tools/call_tool commands spawn their own short-lived MCP client
// against the real stdio server this test wires in. Gated on ACP_MCP_ECHO_BIN.
func TestTesty_McpPassDown(t *testing.T) {
	mcpBin := os.Getenv(acpMcpEchoBinEnv)
	if mcpBin == "" {
		t.Skipf("skipping: set %s to a built mcp-echo-server binary to run (see `make acp-client-e2e`)", acpMcpEchoBinEnv)
	}
	if _, err := os.Stat(mcpBin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", acpMcpEchoBinEnv, mcpBin, err)
	}

	h := newTestyHarness(t)
	h.initialize(t)
	sessID, _, _ := h.newSession(t, []libacp.McpServer{
		{Name: "echo", Command: mcpBin, Args: []string{}, Env: []libacp.EnvVariable{}},
	})

	listResp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt:    testyPrompt(map[string]any{"command": "list_tools", "server": "echo"}),
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, listResp.StopReason)

	listUpdates := h.client.snapshotUpdates()
	require.NotEmpty(t, listUpdates)
	listText := listUpdates[len(listUpdates)-1].Update.Content
	require.NotNil(t, listText)
	assert.Contains(t, listText.Text, "echo")
	assert.Contains(t, listText.Text, "Echoes back the input message")

	callResp, err := h.conn.Prompt(h.ctx, libacp.PromptRequest{
		SessionID: sessID,
		Prompt: testyPrompt(map[string]any{
			"command": "call_tool",
			"server":  "echo",
			"tool":    "echo",
			"params":  map[string]any{"message": "hi from libacp"},
		}),
	})
	require.NoError(t, err)
	assert.Equal(t, libacp.StopReasonEndTurn, callResp.StopReason)

	callUpdates := h.client.snapshotUpdates()
	require.NotEmpty(t, callUpdates)
	callText := callUpdates[len(callUpdates)-1].Update.Content
	require.NotNil(t, callText)
	assert.Contains(t, callText.Text, "OK:")
	assert.Contains(t, callText.Text, "Echo: hi from libacp")
}

func TestTesty_SessionLifecycle(t *testing.T) {
	h := newTestyHarness(t)
	h.initialize(t)
	sessID, cwd, _ := h.newSession(t, nil)

	listResp, err := h.conn.ListSessions(h.ctx, libacp.ListSessionsRequest{Cwd: cwd})
	require.NoError(t, err)
	var found bool
	for _, s := range listResp.Sessions {
		if s.SessionID == sessID {
			found = true
		}
	}
	assert.True(t, found, "session/list must report the session just created for this cwd")

	_, err = h.conn.CloseSession(h.ctx, libacp.CloseSessionRequest{SessionID: sessID})
	require.NoError(t, err)

	_, err = h.conn.DeleteSession(h.ctx, libacp.DeleteSessionRequest{SessionID: sessID})
	require.NoError(t, err)
}
