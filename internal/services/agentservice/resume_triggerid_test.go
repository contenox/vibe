package agentservice_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func twoGatedCalls() taskengine.ChatHistory {
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "write both files", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			{ID: "call-w1", Type: "function", Function: taskengine.FunctionCall{Name: "gate.write", Arguments: `{"path":"/tmp/one"}`}},
			{ID: "call-w2", Type: "function", Function: taskengine.FunctionCall{Name: "gate.write", Arguments: `{"path":"/tmp/two"}`}},
		}},
	}}
}

func TestSystem_TriggerRequestID_SurvivesARestartAndASecondPark(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "trigger-id.db")
	ctx := context.Background()
	const sessionID = "sess-trigger"

	a := newE2EInstance(t, dbPath, awayAsk)
	createSession(t, a.db, sessionID)

	runCtx := agentservice.WithTriggerRequestID(libtracker.WithNewRequestID(ctx), "dispatch/7")
	resp, err := a.agent.Prompt(detachedRun(runCtx), agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: twoGatedCalls(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChain(),
		ChainRef:   "trigger-chain.json",
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	require.Equal(t, "call-w1", resp.SuspendedApprovalID)

	cp, err := a.store.GetChainCheckpoint(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, "dispatch/7", agentservice.TriggerRequestIDOf(cp),
		"the checkpoint carries who is owed this run's outcome")
	require.NotEqual(t, "dispatch/7", cp.RequestID,
		"the request_id column is the run's tracker id, which is why it cannot stand in for the trigger's")
	a.close()

	b := newE2EInstance(t, dbPath, awayAsk)
	defer b.close()
	require.NoError(t, b.hitl.Respond(ctx, "call-w1", true))

	_, err = b.store.GetChainCheckpoint(ctx, "call-w1")
	require.ErrorIs(t, err, libdb.ErrNotFound, "the answered checkpoint is consumed by the resume")

	parked, err := b.store.GetChainCheckpoint(ctx, "call-w2")
	require.NoError(t, err, "the resumed run parked again on the second gated call")
	require.Equal(t, "dispatch/7", agentservice.TriggerRequestIDOf(parked),
		"a run that parks again in another process still owes the same trigger its outcome")
}

func TestUnit_TriggerRequestIDOf_AbsentOnEveryRunNobodyIsWaitingOn(t *testing.T) {
	t.Parallel()
	require.Empty(t, agentservice.TriggerRequestIDOf(nil))
	require.Empty(t, agentservice.TriggerRequestIDOf(&runtimetypes.ChainCheckpoint{}))
	require.Empty(t, agentservice.TriggerRequestIDOf(&runtimetypes.ChainCheckpoint{
		Payload: []byte(`{"checkpoint":{"approvalId":"call-w1"}}`),
	}), "a checkpoint written before the field existed reads as owing nobody")
	require.Empty(t, agentservice.TriggerRequestIDFromContext(context.Background()))
	require.Empty(t, agentservice.TriggerRequestIDFromContext(
		agentservice.WithTriggerRequestID(context.Background(), "   ")))
}
