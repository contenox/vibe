package agentservice

import (
	"context"
	"errors"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

type PromptRequest struct {
	SessionID string
	Input     string
	// Images are attachments riding this turn's user message (vision).
	Images         []taskengine.ImagePart
	InputType      taskengine.DataType
	InputValue     any
	Chain          *taskengine.TaskChainDefinition
	TemplateVars   map[string]string
	ToolsAllowlist []string
	ContextLength  int
	HistoryTrim    int
	Observer       Observer
	AgentsMD       string
	AgentsMDSource string

	// Context carries per-turn artifacts, injected by ComposeUserInput.
	Context map[string]any

	// ChainRef is the chain path for this turn; stamped as provenance, not used for execution.
	ChainRef string
}

type PromptResponse struct {
	Output     any
	OutputType taskengine.DataType
	Steps      []taskengine.CapturedStateUnit
	StopReason StopReason
	// SuspendedApprovalID is set when StopReason is StopSuspended (also the checkpoint key).
	SuspendedApprovalID string
}

type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopCancelled       StopReason = "cancelled"
	// StopSuspended: parked on approval and checkpointed; not a failure.
	StopSuspended StopReason = "suspended"
)

type SessionInfo struct {
	ID           string
	Name         string
	MessageCount int
	IsActive     bool
}

type Observer interface {
	OnStepCompleted(step taskengine.CapturedStateUnit)
}

type NoopObserver struct{}

func (NoopObserver) OnStepCompleted(taskengine.CapturedStateUnit) {}

type AgentCapabilities struct {
	LocalTools      []string
	MCPServers      []string
	SupportsSession bool
}

func InferStopReason(err error, steps []taskengine.CapturedStateUnit) StopReason {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return StopCancelled
		}
		msg := err.Error()
		for _, needle := range []string{"exceeds context length", "token limit", "context_length_exceeded"} {
			if strings.Contains(msg, needle) {
				return StopMaxTokens
			}
		}
	}

	if len(steps) > 0 && steps[len(steps)-1].TaskID == "summarise_failure" {
		return StopMaxTurnRequests
	}

	// A truncated last model step is a max-tokens stop, not end_turn.
	for i := len(steps) - 1; i >= 0; i-- {
		fr := steps[i].FinishReason
		if fr == "" {
			continue
		}
		switch strings.ToLower(fr) {
		case "length", "max_tokens", "max_output_tokens", "model_length":
			return StopMaxTokens
		}
		break // the last model step decides; earlier steps' reasons are history
	}

	return StopEndTurn
}
