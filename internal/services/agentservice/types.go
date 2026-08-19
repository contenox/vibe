package agentservice

import (
	"context"
	"errors"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

type PromptRequest struct {
	SessionID string
	Input     string
	// Images are attachments riding this turn's user message (vision).
	Images []taskengine.ImagePart
	// Audio are attachments riding this turn's user message (audio input).
	Audio          []taskengine.AudioPart
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
	// StopFailed: a task errored and the chain's on_failure handler answered in
	// its place; [RecoveredFailure] names what went wrong.
	StopFailed StopReason = "failed"
)

// FailureSummaryTaskID is the terminal task a chain reaches either by
// spending its loop budget or by any task erroring into on_failure; the two
// arrive identically and cannot be told apart by task id alone.
const FailureSummaryTaskID = "summarise_failure"

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

// RecoveredFailure reports the error a chain's on_failure handler answered in
// place of, or empty when the turn reached [FailureSummaryTaskID] by spending
// its loop budget instead.
func RecoveredFailure(steps []taskengine.CapturedStateUnit) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].TaskID == FailureSummaryTaskID {
			continue
		}
		return steps[i].Error.Error
	}
	return ""
}

func InferStopReason(err error, steps []taskengine.CapturedStateUnit) StopReason {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			for i := len(steps) - 1; i >= 0; i-- {
				step := steps[i]
				if step.TaskHandler == "execute_tool_calls" || step.TaskHandler == "tools" {
					if step.Error.ErrorInternal == nil {
						for _, toolName := range step.ToolNames {
							if toolName == "mission.mission_finish" {
								return StopEndTurn
							}
						}
					}
				}
			}
			return StopCancelled
		}
		msg := err.Error()
		for _, needle := range []string{"exceeds context length", "token limit", "context_length_exceeded"} {
			if strings.Contains(msg, needle) {
				return StopMaxTokens
			}
		}
	}

	if len(steps) > 0 && steps[len(steps)-1].TaskID == FailureSummaryTaskID {
		if RecoveredFailure(steps) != "" {
			return StopFailed
		}
		return StopMaxTurnRequests
	}

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
