package agenthost

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/internal/version"
	"github.com/contenox/beam/libacp"
)

// RecordingHarness is the minimal libacp.Client harness for observing a
// turn: it records every session/update and rejects request-shaped
// callbacks (permission, fs/*, terminal/*). Use DenyingHarness when a
// permission ask must be answered. Safe for concurrent use.
type RecordingHarness struct {
	libacp.UnimplementedClient

	mu      sync.Mutex
	updates []libacp.SessionNotification
}

// SessionUpdate records n. It never returns an error — observing a turn must
// not be able to disturb it.
func (h *RecordingHarness) SessionUpdate(_ context.Context, n libacp.SessionNotification) error {
	h.mu.Lock()
	h.updates = append(h.updates, n)
	h.mu.Unlock()
	return nil
}

// Updates returns a snapshot of every recorded session/update, in arrival order.
func (h *RecordingHarness) Updates() []libacp.SessionNotification {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]libacp.SessionNotification(nil), h.updates...)
}

// AvailableCommands returns the most recent available_commands_update
// recorded; nil means the agent never advertised any.
func (h *RecordingHarness) AvailableCommands() []libacp.AvailableCommand {
	var latest []libacp.AvailableCommand
	seen := false
	for _, n := range h.Updates() {
		if n.Update.SessionUpdate == libacp.SessionUpdateAvailableCommands {
			latest, seen = n.Update.AvailableCommands, true
		}
	}
	if !seen {
		return nil
	}
	return append([]libacp.AvailableCommand(nil), latest...)
}

// MessageText concatenates every agent_message_chunk's text recorded so far.
func (h *RecordingHarness) MessageText() string {
	var sb strings.Builder
	for _, n := range h.Updates() {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// DenyingHarness is RecordingHarness plus one scripted answer: every
// permission ask is denied, preferring reject_once, then reject_always,
// then outcome "cancelled" if neither was offered.
type DenyingHarness struct {
	RecordingHarness

	denyMu sync.Mutex
	denied []string
}

// RequestPermission denies the ask (see the type doc) and records its title.
func (h *DenyingHarness) RequestPermission(_ context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	title := strings.TrimSpace(req.ToolCall.Title)
	if title == "" {
		title = req.ToolCall.ToolCallID
	}
	h.denyMu.Lock()
	h.denied = append(h.denied, title)
	h.denyMu.Unlock()
	for _, kind := range []libacp.PermissionOptionKind{libacp.PermissionRejectOnce, libacp.PermissionRejectAlways} {
		for _, opt := range req.Options {
			if opt.Kind == kind {
				return libacp.RequestPermissionResponse{Outcome: libacp.RequestPermissionOutcome{
					Outcome:  libacp.PermissionOutcomeSelected,
					OptionID: opt.OptionID,
				}}, nil
			}
		}
	}
	return libacp.RequestPermissionResponse{Outcome: libacp.RequestPermissionOutcome{
		Outcome: libacp.PermissionOutcomeCancelled,
	}}, nil
}

// Denied returns the permission asks denied so far, named by tool-call title
// (id when untitled).
func (h *DenyingHarness) Denied() []string {
	h.denyMu.Lock()
	defer h.denyMu.Unlock()
	return append([]string(nil), h.denied...)
}

// TurnRequest describes the one prompt turn DriveTurn drives.
type TurnRequest struct {
	// Cwd is the session working directory. Required, and expected absolute.
	Cwd string

	// Prompt is the user prompt for the driven turn. Required.
	Prompt []libacp.ContentBlock

	// ClientInfo identifies this host to the agent. Defaults to
	// "contenox-agenthost"; an empty Version is filled with the runtime's own.
	ClientInfo *libacp.Implementation

	// ClientCapabilities advertises what harness can serve; the zero value
	// is the honest match for RecordingHarness.
	ClientCapabilities libacp.ClientCapabilities

	// McpServers are MCP servers passed to the agent in session/new.
	// DriveTurn filters them against the agent's mcpCapabilities;
	// kept/dropped are reported on TurnResult.
	McpServers []libacp.McpServer

	// Stderr, if set, receives the spawned agent's stderr as it is written.
	Stderr io.Writer

	// KillGrace, if positive, bounds teardown's wait after stdin-close
	// before killing the agent (see ExternalACPAgent.KillGrace).
	KillGrace time.Duration
}

// TurnResult is what one driven turn produced on the request/response plane;
// the notification plane (streamed chunks) lives on the harness.
type TurnResult struct {
	Initialize libacp.InitializeResponse
	SessionID  libacp.SessionID
	StopReason libacp.StopReason

	// ForwardedMcpServers and DroppedMcpServers name which McpServers
	// reached the agent and which were withheld for unsupported capabilities.
	ForwardedMcpServers []string
	DroppedMcpServers   []string
}

// DriveTurn connects to the external ACP agent agent describes and drives
// one initialize → session/new → session/prompt turn with harness, tearing
// the connection down before returning. A nil error means the agent reached
// a terminal stopReason and teardown closed cleanly.
func DriveTurn(ctx context.Context, agent *runtimetypes.Agent, harness libacp.Client, req TurnRequest) (*TurnResult, error) {
	if req.Cwd == "" {
		return nil, fmt.Errorf("agenthost: TurnRequest.Cwd is required (ACP session/new needs a working directory)")
	}
	if len(req.Prompt) == 0 {
		return nil, fmt.Errorf("agenthost: TurnRequest.Prompt is required")
	}
	cfg, err := agent.ExternalACPConfig()
	if err != nil {
		return nil, fmt.Errorf("agenthost: resolve agent %q: %w", agent.Name, err)
	}

	// Default the sandbox workspace (Config.Cwd) to this turn's session cwd
	// when the agent has none registered; an explicit registered Cwd wins.
	if cfg.Cwd == "" {
		cfg.Cwd = req.Cwd
	}

	host := &ExternalACPAgent{Config: *cfg, Stderr: req.Stderr, KillGrace: req.KillGrace}
	handle, err := host.Connect(ctx, harness)
	if err != nil {
		return nil, err
	}
	// Close is idempotent: the deferred call cleans up the error paths, the
	// explicit one at the end makes teardown failures part of the result.
	defer handle.Close()

	clientInfo := &libacp.Implementation{Name: "contenox-agenthost"}
	if req.ClientInfo != nil {
		info := *req.ClientInfo
		clientInfo = &info
	}
	if clientInfo.Version == "" {
		clientInfo.Version = version.Get()
	}
	init, err := handle.Conn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion:    libacp.ProtocolVersion,
		ClientCapabilities: req.ClientCapabilities,
		ClientInfo:         clientInfo,
	})
	if err != nil {
		return nil, fmt.Errorf("agenthost: initialize agent %q: %w", agent.Name, err)
	}
	if init.ProtocolVersion != libacp.ProtocolVersion {
		return nil, fmt.Errorf("agenthost: agent %q negotiated unsupported protocol version %d (host speaks %d)",
			agent.Name, init.ProtocolVersion, libacp.ProtocolVersion)
	}

	mcpServers, droppedMcp := filterMcpServersByCapabilities(req.McpServers, init.AgentCapabilities.McpCapabilities)
	if mcpServers == nil {
		mcpServers = []libacp.McpServer{}
	}
	forwardedMcp := make([]string, 0, len(mcpServers))
	for _, srv := range mcpServers {
		forwardedMcp = append(forwardedMcp, srv.Name)
	}

	sess, err := handle.Conn.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        req.Cwd,
		McpServers: mcpServers,
	})
	if err != nil {
		return nil, fmt.Errorf("agenthost: session/new against agent %q: %w", agent.Name, err)
	}

	resp, err := handle.Conn.Prompt(ctx, libacp.PromptRequest{
		SessionID: sess.SessionID,
		Prompt:    req.Prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("agenthost: prompt against agent %q: %w", agent.Name, err)
	}

	if err := handle.Close(); err != nil {
		return nil, fmt.Errorf("agenthost: close agent %q after turn: %w", agent.Name, err)
	}
	return &TurnResult{
		Initialize:          init,
		SessionID:           sess.SessionID,
		StopReason:          resp.StopReason,
		ForwardedMcpServers: forwardedMcp,
		DroppedMcpServers:   droppedMcp,
	}, nil
}
