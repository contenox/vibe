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
	// Images are attachments riding this turn's user message (vision). They
	// travel on taskengine.Message.Images, persist with the session history,
	// and route the request to CanVision providers via llmresolver.
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

	// Per-turn context artifacts from Beam ({artifacts: []ChatContextArtifact-like}),
	// injected into the model-visible user message by ComposeUserInput.
	Context map[string]any

	// ChainRef is the chain path used for this turn (e.g. "default-chain.json").
	// Stamped onto persisted messages as turn provenance; not used for execution.
	ChainRef string
}

type PromptResponse struct {
	Output     any
	OutputType taskengine.DataType
	Steps      []taskengine.CapturedStateUnit
	StopReason StopReason
	// SuspendedApprovalID is set when StopReason is StopSuspended: the
	// approval whose verdict resumes the run (== the checkpoint key —
	// hitlservice.Respond on it triggers ResumeFromCheckpoint via the
	// registered hook).
	SuspendedApprovalID string
}

type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopCancelled       StopReason = "cancelled"
	// StopSuspended: the run parked on a human approval past the fast window
	// and was checkpointed (S6). Not a failure — the run continues once the
	// approval is answered, in whichever process answers it.
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

	return StopEndTurn
}
