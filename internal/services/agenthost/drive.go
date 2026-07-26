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

// RecordingHarness is the minimal libacp.Client harness for driving an agent
// whose turn only needs to be observed, not interacted with: it records every
// session/update notification and, via the embedded UnimplementedClient,
// rejects the request-shaped callbacks (permission, fs/*, terminal/*). A turn
// that needs a permission answered needs a scripted harness instead (see
// DenyingHarness for the deny-everything one) — this one exists so a caller
// can read an agent's streamed reply after the fact.
//
// Safe for concurrent use: updates arrive on the connection's read-loop
// goroutine while callers read snapshots from their own.
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

// Updates returns a snapshot of every recorded session/update, in arrival
// order.
func (h *RecordingHarness) Updates() []libacp.SessionNotification {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]libacp.SessionNotification(nil), h.updates...)
}

// AvailableCommands returns the agent's advertised slash-command set — the
// most recent available_commands_update recorded, since each update is a full
// replacement list per the spec. Nil means the agent never advertised any.
// This surfaces the hosted agent's command menu to callers (`agent check`
// prints it); merging it with contenox's own acpsvc command set is the
// re-exposure layer's concern, not this package's.
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

// MessageText concatenates the text of every agent_message_chunk recorded so
// far: the agent's streamed reply as one string.
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
// session/request_permission ask is DENIED, by selecting the agent's own
// reject_once option (reject_always as the fallback; outcome "cancelled" when
// the agent offered no reject option at all). It exists for observe-only
// drivers that must still be interactable enough for a spec-correct agent to
// finish its turn gracefully: the bare RecordingHarness answers a permission
// ask with MethodNotFound, which agents surface as a BROKEN CLIENT and derail
// their reply into an error narrative (`contenox agent check` hit exactly
// this), while a clean denial lets the agent report the denial and end the
// turn on its own terms. Denied() names each ask so a caller can surface what
// the agent tried.
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

// Denied returns a snapshot of the permission asks denied so far, in arrival
// order, each named by its tool-call title (id when untitled).
func (h *DenyingHarness) Denied() []string {
	h.denyMu.Lock()
	defer h.denyMu.Unlock()
	return append([]string(nil), h.denied...)
}

// TurnRequest describes the one prompt turn DriveTurn drives.
type TurnRequest struct {
	// Cwd is the session working directory. Required: ACP's session/new
	// requires one, and spec-correct agents expect it to be absolute.
	Cwd string

	// Prompt is the user prompt for the driven turn. Required.
	Prompt []libacp.ContentBlock

	// ClientInfo identifies this host to the agent. Defaults to a
	// "contenox-agenthost" identity when nil. The ACP spec requires both
	// name and version, and real-world agents (the claude-code-acp adapter)
	// hard-reject an initialize without a version — so DriveTurn fills an
	// empty Version with the runtime's own before sending.
	ClientInfo *libacp.Implementation

	// ClientCapabilities advertises what the supplied harness can actually
	// serve. The zero value (nothing advertised) is the honest match for
	// RecordingHarness.
	ClientCapabilities libacp.ClientCapabilities

	// McpServers are MCP servers passed down to the agent in session/new —
	// typically the resolved form of the agent's mcp_servers allowlist (see
	// ResolveForwardedMcpServers). DriveTurn filters them against the
	// agent's initialize-advertised mcpCapabilities before sending; what was
	// kept and dropped is reported on TurnResult.
	McpServers []libacp.McpServer

	// Stderr, if set, receives the spawned agent's stderr as it is written —
	// pass a buffer so a failing turn is diagnosable without rerunning.
	Stderr io.Writer

	// KillGrace, if positive, bounds how long teardown waits for the agent
	// to exit on stdin-close before killing it (see
	// ExternalACPAgent.KillGrace). Set it short for persistent agents that
	// never exit on their own.
	KillGrace time.Duration
}

// TurnResult is what one driven turn produced on the request/response plane.
// The notification plane (streamed chunks, tool calls) lives on the harness —
// see RecordingHarness.
type TurnResult struct {
	Initialize libacp.InitializeResponse
	SessionID  libacp.SessionID
	StopReason libacp.StopReason

	// ForwardedMcpServers and DroppedMcpServers name which of
	// TurnRequest.McpServers actually reached the agent in session/new and
	// which were withheld because the agent's mcpCapabilities can't consume
	// their transport. Callers surface these so a user who allowlisted a
	// server learns when the agent never saw it.
	ForwardedMcpServers []string
	DroppedMcpServers   []string
}

// DriveTurn composes a resolved agents row with the host: it connects to the
// external ACP agent the row describes and drives one full
// initialize → session/new → session/prompt turn against it with harness,
// tearing the connection down before returning. Resolving the row (by name,
// via the registry service) stays with the caller — this package remains
// registry-agnostic; it only consumes the resolved *runtimetypes.Agent.
//
// A nil error means the whole loop closed: the agent answered the prompt with
// a terminal stopReason and the spawned process tore down cleanly (Close
// errors are returned, not swallowed). Everything the agent streamed during
// the turn is on the harness, which DriveTurn passes through untouched per
// this package's harness seam.
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

	// The agent is spawned inside the sandbox, whose workspace is Config.Cwd (see
	// buildAgentCmd). An agent is usually registered by command alone, with no
	// Cwd — its working directory is chosen per turn — so default the sandbox
	// workspace to this turn's session cwd (already validated non-empty above).
	// That both satisfies the wall's non-empty-workspace requirement and pins the
	// spawned process to the very directory the ACP session works in. An explicit
	// registered Cwd, if set, still wins.
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
