package agentservice_test

// InferStopReason must distinguish a truncated success (finish reason in
// the "length" class) from a normal end of turn.

import (
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
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

// A chain reaches summarise_failure both by spending its loop budget and by a
// task erroring into on_failure. Only the first is max_turn_requests; reporting
// the second that way told operators to /clear over a provider 404 and hid the
// error behind the handler's progress summary.
func TestUnit_InferStopReason_ErroredRecoveryIsNotABudgetStop(t *testing.T) {
	const cause = "vertex API returned non-200 status for stream: 404"
	steps := []taskengine.CapturedStateUnit{
		{TaskID: "coding_chat", TaskHandler: "chat_completion"},
		{TaskID: "coding_recovery", TaskHandler: "chat_completion",
			Error: taskengine.ErrorResponse{Error: cause}},
		{TaskID: agentservice.FailureSummaryTaskID, TaskHandler: "chat_completion"},
	}
	require.Equal(t, agentservice.StopFailed, agentservice.InferStopReason(nil, steps))
	require.Equal(t, cause, agentservice.RecoveredFailure(steps),
		"the originating error must survive for the operator to read")
}

// The budget path is unchanged: the step before the summary succeeded, so
// nothing errored and max_turn_requests is the honest answer.
func TestUnit_InferStopReason_SpentBudgetStaysMaxTurnRequests(t *testing.T) {
	steps := []taskengine.CapturedStateUnit{
		{TaskID: "coding_chat", TaskHandler: "chat_completion"},
		{TaskID: "coding_recovery", TaskHandler: "chat_completion"},
		{TaskID: agentservice.FailureSummaryTaskID, TaskHandler: "chat_completion"},
	}
	require.Equal(t, agentservice.StopMaxTurnRequests, agentservice.InferStopReason(nil, steps))
	require.Empty(t, agentservice.RecoveredFailure(steps))
}

// The last model step decides; an earlier truncation is overridden by a
// later normal finish.
func TestUnit_InferStopReason_LastModelStepDecides(t *testing.T) {
	steps := []taskengine.CapturedStateUnit{stepWithFinish("length"), stepWithFinish("stop")}
	require.Equal(t, agentservice.StopEndTurn, agentservice.InferStopReason(nil, steps))

	steps = []taskengine.CapturedStateUnit{stepWithFinish("stop"), stepWithFinish("length")}
	require.Equal(t, agentservice.StopMaxTokens, agentservice.InferStopReason(nil, steps))

	// Trailing non-model steps (no finish reason) are skipped, not decisive.
	steps = []taskengine.CapturedStateUnit{stepWithFinish("length"), {TaskID: "tool", TaskHandler: "execute_tool_calls"}}
	require.Equal(t, agentservice.StopMaxTokens, agentservice.InferStopReason(nil, steps))
}
