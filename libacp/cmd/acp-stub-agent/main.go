// Command acp-stub-agent is a hermetic ACP Agent used to validate libacp's
// agent-side wire dispatch against ACP conformance clients, without any LLM
// backend. It speaks ACP v1 over stdio like the production `contenox
// acp`/`acpx` commands, but every response is deterministic and in-memory.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/contenox/contenox/libacp"
)

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }

func main() {
	conn := libacp.NewAgentSideConnection(stdio{}, func(c *libacp.AgentSideConnection) libacp.Agent {
		return newStubAgent(c)
	})
	if err := conn.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "acp-stub-agent: %v\n", err)
		os.Exit(1)
	}
}

const (
	stubAuthMethodID = "stub-auth"

	stubModeCode = "code"
	stubModeAsk  = "ask"

	stubModelFast  = "stub-model-fast"
	stubModelSmart = "stub-model-smart"

	stubConfigVerbosityID = "stub-verbosity"
	stubVerbosityLow      = "low"
	stubVerbosityHigh     = "high"
)

type stubSession struct {
	cwd                   string
	additionalDirectories []string
	modeID                string
	modelID               string
	verbosity             string
}

type stubAgent struct {
	libacp.UnimplementedAgent

	conn *libacp.AgentSideConnection

	mu       sync.Mutex
	sessions map[libacp.SessionID]*stubSession

	nextID      atomic.Int64
	nextToolID  atomic.Int64
	authedOnce  atomic.Bool
	loggedOutOK atomic.Bool

	advertiseCommands bool

	advertiseConfigOptions         bool
	advertiseConfigOptionsAfterNew bool

	advertiseModes bool

	advertiseModels bool

	useTerminal    bool
	clientTerminal atomic.Bool
}

func newStubAgent(c *libacp.AgentSideConnection) *stubAgent {
	return &stubAgent{
		conn:                           c,
		sessions:                       make(map[libacp.SessionID]*stubSession),
		advertiseCommands:              os.Getenv("ACP_STUB_ADVERTISE_COMMANDS") == "1",
		advertiseConfigOptions:         os.Getenv("ACP_STUB_ADVERTISE_CONFIG_OPTIONS") == "1",
		advertiseConfigOptionsAfterNew: os.Getenv("ACP_STUB_CONFIG_OPTIONS_AFTER_NEW") == "1",
		advertiseModes:                 os.Getenv("ACP_STUB_ADVERTISE_MODES") == "1",
		advertiseModels:                os.Getenv("ACP_STUB_ADVERTISE_MODELS") == "1",
		useTerminal:                    os.Getenv("ACP_STUB_USE_TERMINAL") == "1",
	}
}

const stubTerminalMarker = "stub-terminal-42"

func stubModeState() *libacp.SessionModeState {
	return &libacp.SessionModeState{
		CurrentModeID: stubModeCode,
		AvailableModes: []libacp.SessionMode{
			{ID: stubModeCode, Name: "Code", Description: "Full tool access"},
			{ID: stubModeAsk, Name: "Ask", Description: "Read-only, asks before acting"},
		},
	}
}

func stubModelState() *libacp.SessionModelState {
	return &libacp.SessionModelState{
		CurrentModelID: stubModelFast,
		AvailableModels: []libacp.ModelInfo{
			{ID: stubModelFast, Name: "Fast", Description: "Lower latency, smaller model"},
			{ID: stubModelSmart, Name: "Smart", Description: "Higher quality, slower model"},
		},
	}
}

func stubConfigOptions(current string) []libacp.SessionConfigOption {
	if current == "" {
		current = stubVerbosityLow
	}
	return []libacp.SessionConfigOption{{
		ID:           stubConfigVerbosityID,
		Name:         "Verbosity",
		Description:  "Stub: how chatty the stub agent is.",
		Category:     "stub",
		Type:         libacp.SessionConfigOptionTypeSelect,
		CurrentValue: current,
		Options: libacp.NewSessionConfigValues([]libacp.SessionConfigValue{
			{Value: stubVerbosityLow, Name: "Low"},
			{Value: stubVerbosityHigh, Name: "High"},
		}),
	}}
}

func stubAdvertisedCommands() []libacp.AvailableCommand {
	return []libacp.AvailableCommand{
		{Name: "review", Description: "Stub: review the current changes."},
		{Name: "explain", Description: "Stub: explain a file.", Input: &libacp.AvailableCommandInput{Hint: "[path]"}},
	}
}

func negotiateProtocolVersion(client int) int {
	if client >= 1 && client <= libacp.ProtocolVersion {
		return client
	}
	return libacp.ProtocolVersion
}

func (a *stubAgent) Initialize(_ context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	a.clientTerminal.Store(req.ClientCapabilities.Terminal)
	return libacp.InitializeResponse{
		ProtocolVersion: negotiateProtocolVersion(req.ProtocolVersion),
		AgentInfo: &libacp.Implementation{
			Name:    "acp-stub-agent",
			Title:   "libacp conformance stub",
			Version: "0.0.1",
		},
		AgentCapabilities: libacp.AgentCapabilities{
			LoadSession: false,
			PromptCapabilities: libacp.PromptCapabilities{
				Image:           false,
				Audio:           false,
				EmbeddedContext: req.ClientCapabilities.FS.ReadTextFile,
			},
			McpCapabilities: libacp.McpCapabilities{
				HTTP: false,
				SSE:  false,
			},
			SessionCapabilities: libacp.SessionCapabilities{
				AdditionalDirectories: &struct{}{},
			},
			Auth: libacp.AgentAuthCapabilities{
				Logout: &libacp.LogoutCapabilities{},
			},
		},
		AuthMethods: []libacp.AuthMethod{
			{
				ID:          stubAuthMethodID,
				Name:        "Stub Auth",
				Description: "Always-succeeds auth method for conformance testing.",
			},
		},
	}, nil
}

func (a *stubAgent) Authenticate(_ context.Context, _ libacp.AuthenticateRequest) (libacp.AuthenticateResponse, error) {
	a.authedOnce.Store(true)
	return libacp.AuthenticateResponse{}, nil
}

func (a *stubAgent) Logout(_ context.Context, _ libacp.LogoutRequest) (libacp.LogoutResponse, error) {
	a.loggedOutOK.Store(true)
	return libacp.LogoutResponse{}, nil
}

func (a *stubAgent) NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	id := libacp.SessionID(fmt.Sprintf("stub-session-%d", a.nextID.Add(1)))

	a.mu.Lock()
	a.sessions[id] = &stubSession{
		cwd:                   req.Cwd,
		additionalDirectories: req.AdditionalDirectories,
		modeID:                stubModeCode,
		modelID:               stubModelFast,
		verbosity:             stubVerbosityLow,
	}
	a.mu.Unlock()

	// Deferred via AfterResponse so the update reaches the client strictly
	// after session/new — a conformant client would otherwise drop it as
	// referencing an unknown session id.
	if a.advertiseCommands {
		libacp.AfterResponse(ctx, func() {
			_ = a.conn.SessionUpdate(libacp.SessionNotification{
				SessionID: id,
				Update: libacp.SessionUpdate{
					SessionUpdate:     libacp.SessionUpdateAvailableCommands,
					AvailableCommands: stubAdvertisedCommands(),
				},
			})
		})
	}

	// Emitted as a deferred config_option_update after session/new (options
	// absent from the response itself) to exercise the pre-bind caching path.
	if a.advertiseConfigOptionsAfterNew {
		libacp.AfterResponse(ctx, func() {
			_ = a.conn.SessionUpdate(libacp.SessionNotification{
				SessionID: id,
				Update: libacp.SessionUpdate{
					SessionUpdate: libacp.SessionUpdateConfigOption,
					ConfigOptions: stubConfigOptions(stubVerbosityLow),
				},
			})
		})
	}

	resp := libacp.NewSessionResponse{SessionID: id}
	// Carried IN the session/new response (opt-in, default off).
	if a.advertiseModes {
		resp.Modes = stubModeState()
	}
	if a.advertiseModels {
		resp.Models = stubModelState()
	}
	if a.advertiseConfigOptions {
		resp.ConfigOptions = stubConfigOptions(stubVerbosityLow)
	}
	return resp, nil
}

// SetSessionConfigOption honors the deterministic "verbosity" option when
// advertised, reporting MethodNotFound otherwise.
func (a *stubAgent) SetSessionConfigOption(ctx context.Context, req libacp.SetSessionConfigOptionRequest) (libacp.SetSessionConfigOptionResponse, error) {
	if !a.advertiseConfigOptions {
		return libacp.SetSessionConfigOptionResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetConfigOption)
	}
	if req.ConfigID != stubConfigVerbosityID {
		return libacp.SetSessionConfigOptionResponse{}, libacp.InvalidParams("unknown configId: " + req.ConfigID)
	}
	value := req.Value.AsString()
	if value != stubVerbosityLow && value != stubVerbosityHigh {
		return libacp.SetSessionConfigOptionResponse{}, libacp.InvalidParams("unknown value: " + value)
	}

	a.mu.Lock()
	sess, ok := a.sessions[req.SessionID]
	if ok {
		sess.verbosity = value
	}
	a.mu.Unlock()
	if !ok {
		return libacp.SetSessionConfigOptionResponse{}, libacp.InvalidParams("unknown sessionId: " + string(req.SessionID))
	}

	// Deferred so the confirming notification reaches the wire after this response.
	libacp.AfterResponse(ctx, func() {
		_ = a.conn.SessionUpdate(libacp.SessionNotification{
			SessionID: req.SessionID,
			Update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateConfigOption,
				ConfigOptions: stubConfigOptions(value),
			},
		})
	})

	return libacp.SetSessionConfigOptionResponse{ConfigOptions: stubConfigOptions(value)}, nil
}

func (a *stubAgent) SetSessionMode(ctx context.Context, req libacp.SetSessionModeRequest) (libacp.SetSessionModeResponse, error) {
	a.mu.Lock()
	sess, ok := a.sessions[req.SessionID]
	if ok {
		sess.modeID = req.ModeID
	}
	a.mu.Unlock()
	if !ok {
		return libacp.SetSessionModeResponse{}, libacp.InvalidParams("unknown sessionId: " + string(req.SessionID))
	}

	// Deferred so the confirming notification reaches the wire after this response.
	libacp.AfterResponse(ctx, func() {
		_ = a.conn.SessionUpdate(libacp.SessionNotification{
			SessionID: req.SessionID,
			Update: libacp.SessionUpdate{
				SessionUpdate: libacp.SessionUpdateCurrentMode,
				CurrentModeID: req.ModeID,
			},
		})
	})

	return libacp.SetSessionModeResponse{}, nil
}

// SetSessionModel honors the deterministic model picker when advertised,
// reporting MethodNotFound otherwise.
func (a *stubAgent) SetSessionModel(_ context.Context, req libacp.SetSessionModelRequest) (libacp.SetSessionModelResponse, error) {
	if !a.advertiseModels {
		return libacp.SetSessionModelResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetModel)
	}
	if req.ModelID != stubModelFast && req.ModelID != stubModelSmart {
		return libacp.SetSessionModelResponse{}, libacp.InvalidParams("unknown modelId: " + req.ModelID)
	}

	a.mu.Lock()
	sess, ok := a.sessions[req.SessionID]
	if ok {
		sess.modelID = req.ModelID
	}
	a.mu.Unlock()
	if !ok {
		return libacp.SetSessionModelResponse{}, libacp.InvalidParams("unknown sessionId: " + string(req.SessionID))
	}

	return libacp.SetSessionModelResponse{}, nil
}

func (a *stubAgent) sessionCwd(id libacp.SessionID) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sess, ok := a.sessions[id]; ok && sess.cwd != "" {
		return sess.cwd
	}
	return os.TempDir()
}

func promptText(req libacp.PromptRequest) string {
	var sb strings.Builder
	for _, block := range req.Prompt {
		if block.Type == string(libacp.ContentKindText) {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

func (a *stubAgent) Prompt(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	text := promptText(req)
	switch {
	case strings.Contains(text, "session_updates"):
		return a.promptStreaming(ctx, req)
	case strings.Contains(text, "callbacks"):
		return a.promptCallbacks(ctx, req)
	case strings.Contains(text, gatedActionTrigger):
		return a.promptGatedAction(ctx, req)
	case a.useTerminal:
		return a.promptTerminal(ctx, req)
	default:
		return a.promptPlain(ctx, req)
	}
}

func (a *stubAgent) promptTerminal(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	if !a.clientTerminal.Load() {
		return a.terminalReport(req, "termcap=false")
	}
	if strings.Contains(promptText(req), "terminal_kill") {
		return a.promptTerminalKill(ctx, req)
	}

	createResp, err := a.conn.CreateTerminal(ctx, libacp.CreateTerminalRequest{
		SessionID: req.SessionID,
		// Full command line in `command` with no args, piped, so the round
		// trip proves word-splitting and pipes survive the bridge's shell wrap.
		Command: "echo stub-terminal-$((6*7)) | cat",
	})
	if err != nil {
		return a.terminalReport(req, "termcap=true create-error="+err.Error())
	}
	termID := createResp.TerminalID

	exit, err := a.conn.WaitForTerminalExit(ctx, libacp.WaitForTerminalExitRequest{
		SessionID:  req.SessionID,
		TerminalID: termID,
	})
	if err != nil {
		return a.terminalReport(req, "termcap=true wait-error="+err.Error())
	}

	out, err := a.conn.TerminalOutput(ctx, libacp.TerminalOutputRequest{
		SessionID:  req.SessionID,
		TerminalID: termID,
	})
	if err != nil {
		return a.terminalReport(req, "termcap=true output-error="+err.Error())
	}

	if _, err := a.conn.ReleaseTerminal(ctx, libacp.ReleaseTerminalRequest{
		SessionID:  req.SessionID,
		TerminalID: termID,
	}); err != nil {
		return a.terminalReport(req, "termcap=true release-error="+err.Error())
	}

	exitStr := "nil"
	if exit.ExitCode != nil {
		exitStr = fmt.Sprintf("%d", *exit.ExitCode)
	} else if exit.Signal != nil {
		exitStr = "signal:" + *exit.Signal
	}
	report := fmt.Sprintf("termcap=true exit=%s truncated=%v output=%q", exitStr, out.Truncated, out.Output)
	return a.terminalReport(req, report)
}

func (a *stubAgent) promptTerminalKill(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	createResp, err := a.conn.CreateTerminal(ctx, libacp.CreateTerminalRequest{
		SessionID: req.SessionID,
		Command:   "sh",
		Args:      []string{"-c", "sleep 30; echo should-not-appear"},
	})
	if err != nil {
		return a.terminalReport(req, "termcap=true kill create-error="+err.Error())
	}
	termID := createResp.TerminalID

	if _, err := a.conn.KillTerminal(ctx, libacp.KillTerminalRequest{
		SessionID:  req.SessionID,
		TerminalID: termID,
	}); err != nil {
		return a.terminalReport(req, "termcap=true kill kill-error="+err.Error())
	}

	exit, err := a.conn.WaitForTerminalExit(ctx, libacp.WaitForTerminalExitRequest{
		SessionID:  req.SessionID,
		TerminalID: termID,
	})
	if err != nil {
		return a.terminalReport(req, "termcap=true kill wait-error="+err.Error())
	}

	if _, err := a.conn.ReleaseTerminal(ctx, libacp.ReleaseTerminalRequest{
		SessionID:  req.SessionID,
		TerminalID: termID,
	}); err != nil {
		return a.terminalReport(req, "termcap=true kill release-error="+err.Error())
	}

	exitStr := "nil"
	if exit.ExitCode != nil {
		exitStr = fmt.Sprintf("%d", *exit.ExitCode)
	} else if exit.Signal != nil {
		exitStr = "signal:" + *exit.Signal
	}
	return a.terminalReport(req, "kill exit="+exitStr)
}

func (a *stubAgent) terminalReport(req libacp.PromptRequest, msg string) (libacp.PromptResponse, error) {
	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("terminal-scenario " + msg),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

const (
	gatedActionTrigger = "gated_action"

	envGatedToolsName = "ACP_STUB_GATED_TOOLS_NAME"
	envGatedToolName  = "ACP_STUB_GATED_TOOL_NAME"
	envGatedArgsJSON  = "ACP_STUB_GATED_ARGS_JSON"

	envGatedReportPath = "ACP_STUB_GATED_REPORT_PATH"
)

const gatedActionReport = "gated-action outcome="

func (a *stubAgent) promptGatedAction(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	toolsName := os.Getenv(envGatedToolsName)
	toolName := os.Getenv(envGatedToolName)
	meta, err := json.Marshal(map[string]string{"toolsName": toolsName, "toolName": toolName})
	if err != nil {
		return libacp.PromptResponse{}, err
	}
	var rawInput json.RawMessage
	if args := strings.TrimSpace(os.Getenv(envGatedArgsJSON)); args != "" {
		rawInput = json.RawMessage(args)
	}

	toolCallID := fmt.Sprintf("stub-gated-%d", a.nextToolID.Add(1))
	permResp, err := a.conn.RequestPermission(ctx, libacp.RequestPermissionRequest{
		SessionID: req.SessionID,
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: toolCallID,
			Title:      strings.TrimSpace(toolsName + "." + toolName),
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusPending,
			RawInput:   rawInput,
			Meta:       meta,
		},
		Options: []libacp.PermissionOption{
			{OptionID: "allow-once", Name: "Allow once", Kind: libacp.PermissionAllowOnce},
			{OptionID: "reject-once", Name: "Reject once", Kind: libacp.PermissionRejectOnce},
		},
		Meta: meta,
	})
	if err != nil {
		if ctx.Err() != nil {
			return libacp.PromptResponse{}, ctx.Err()
		}
		return libacp.PromptResponse{}, err
	}

	report := gatedActionReport + string(permResp.Outcome.Outcome)
	if permResp.Outcome.OptionID != "" {
		report += " option=" + permResp.Outcome.OptionID
	}
	if path := strings.TrimSpace(os.Getenv(envGatedReportPath)); path != "" {
		if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
			return libacp.PromptResponse{}, err
		}
	}
	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk(report),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	if permResp.Outcome.Outcome == libacp.PermissionOutcomeCancelled {
		return libacp.PromptResponse{StopReason: libacp.StopReasonRefusal}, nil
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func (a *stubAgent) promptPlain(_ context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("ack"),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func (a *stubAgent) promptStreaming(_ context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	toolCallID := fmt.Sprintf("stub-tool-%d", a.nextToolID.Add(1))

	updates := []libacp.SessionUpdate{
		libacp.NewAgentMessageChunk("running scenario..."),
		{
			SessionUpdate: libacp.SessionUpdateToolCall,
			ToolCallID:    toolCallID,
			Title:         "stub tool call",
			Kind:          libacp.ToolKindExecute,
			Status:        libacp.ToolCallStatusInProgress,
		},
		{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    toolCallID,
			Status:        libacp.ToolCallStatusCompleted,
		},
		libacp.NewAgentMessageChunk("done"),
	}
	for _, u := range updates {
		if err := a.conn.SessionUpdate(libacp.SessionNotification{SessionID: req.SessionID, Update: u}); err != nil {
			return libacp.PromptResponse{}, err
		}
	}
	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}

func (a *stubAgent) promptCallbacks(ctx context.Context, req libacp.PromptRequest) (libacp.PromptResponse, error) {
	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update:    libacp.NewAgentMessageChunk("requesting permission..."),
	}); err != nil {
		return libacp.PromptResponse{}, err
	}

	toolCallID := fmt.Sprintf("stub-tool-%d", a.nextToolID.Add(1))
	permResp, err := a.conn.RequestPermission(ctx, libacp.RequestPermissionRequest{
		SessionID: req.SessionID,
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: toolCallID,
			Title:      "write scratch file",
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusPending,
		},
		Options: []libacp.PermissionOption{
			{OptionID: "allow-once", Name: "Allow once", Kind: libacp.PermissionAllowOnce},
			{OptionID: "reject-once", Name: "Reject once", Kind: libacp.PermissionRejectOnce},
		},
	})
	if err != nil {
		// A session/cancel mid-call surfaces as ctx cancellation; propagate it
		// so the turn resolves with stopReason "cancelled" instead of an error.
		if ctx.Err() != nil {
			return libacp.PromptResponse{}, ctx.Err()
		}
		return libacp.PromptResponse{}, err
	}

	if permResp.Outcome.Outcome == libacp.PermissionOutcomeCancelled {
		return libacp.PromptResponse{StopReason: libacp.StopReasonRefusal}, nil
	}

	path := a.sessionCwd(req.SessionID) + "/acp-stub-scratch.txt"
	if _, err := a.conn.WriteTextFile(ctx, libacp.WriteTextFileRequest{
		SessionID: req.SessionID,
		Path:      path,
		Content:   "acp-stub-agent scratch content\n",
	}); err != nil {
		if ctx.Err() != nil {
			return libacp.PromptResponse{}, ctx.Err()
		}
		return libacp.PromptResponse{}, err
	}

	if _, err := a.conn.ReadTextFile(ctx, libacp.ReadTextFileRequest{
		SessionID: req.SessionID,
		Path:      path,
	}); err != nil {
		if ctx.Err() != nil {
			return libacp.PromptResponse{}, ctx.Err()
		}
		return libacp.PromptResponse{}, err
	}

	if err := a.conn.SessionUpdate(libacp.SessionNotification{
		SessionID: req.SessionID,
		Update: libacp.SessionUpdate{
			SessionUpdate: libacp.SessionUpdateToolCallUpdate,
			ToolCallID:    toolCallID,
			Status:        libacp.ToolCallStatusCompleted,
		},
	}); err != nil {
		return libacp.PromptResponse{}, err
	}

	return libacp.PromptResponse{StopReason: libacp.StopReasonEndTurn}, nil
}
