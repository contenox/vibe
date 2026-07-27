package agentservice_test

// InferStopReason must tell a truncated SUCCESS from a normal end of turn:
// err == nil with the last model step's finish reason in the "length" class →
// StopMaxTokens, in every provider's vocabulary.

import (
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/stretchr/testify/require"
)

func stepWithFinish(fr string) taskengine.CapturedStateUnit {
	return taskengine.CapturedStateUnit{TaskID: "chat", TaskHandler: "chat_completion", FinishReason: fr}
}

func TestUnit_InferStopReason_TruncatedSuccessIsMaxTokens(t *testing.T) {
	for _, fr := range []string{"length", "max_tokens", "MAX_TOKENS", "max_output_tokens"} {
		got := agentservice.InferStopReason(nil, []taskengine.CapturedStateUnit{stepWithFinish(fr)})
		require.Equal(t, agentservice.StopMaxTokens, got, "finish reason %q must read as max-tokens", fr)
	}
}

func TestUnit_InferStopReason_NormalFinishStaysEndTurn(t *testing.T) {
	for _, fr := range []string{"stop", "end_turn", "tool_calls", "STOP", ""} {
		got := agentservice.InferStopReason(nil, []taskengine.CapturedStateUnit{stepWithFinish(fr)})
		require.Equal(t, agentservice.StopEndTurn, got, "finish reason %q must stay end_turn", fr)
	}
}

// The LAST model step decides: an earlier truncation followed by a normal
// completion (e.g. a retry or continuation) is not a truncated turn.
func TestUnit_InferStopReason_LastModelStepDecides(t *testing.T) {
	steps := []taskengine.CapturedStateUnit{stepWithFinish("length"), stepWithFinish("stop")}
	require.Equal(t, agentservice.StopEndTurn, agentservice.InferStopReason(nil, steps))

	steps = []taskengine.CapturedStateUnit{stepWithFinish("stop"), stepWithFinish("length")}
	require.Equal(t, agentservice.StopMaxTokens, agentservice.InferStopReason(nil, steps))

	// Trailing non-model steps (no finish reason) are skipped, not decisive.
	steps = []taskengine.CapturedStateUnit{stepWithFinish("length"), {TaskID: "tool", TaskHandler: "execute_tool_calls"}}
	require.Equal(t, agentservice.StopMaxTokens, agentservice.InferStopReason(nil, steps))
}
