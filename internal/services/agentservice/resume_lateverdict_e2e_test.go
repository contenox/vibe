package agentservice_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func (e *e2eInnerTools) execCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.execs)
}

func lateAsk(release <-chan struct{}, approved bool) func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
	return func(ctx context.Context, _ hitlservice.ApprovalRequest) (bool, error) {
		<-release
		return approved, nil
	}
}

// TestSystem_S6Gate_LateVerdictThroughTheSameAskChannel_ResumesWithoutAnExternalRespond pins that a verdict delivered late through the same in-memory ask() channel, not a separate hitlservice.Respond call, still resumes the checkpointed run and completes the turn visibly.
func TestSystem_S6Gate_LateVerdictThroughTheSameAskChannel_ResumesWithoutAnExternalRespond(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "s6-gate-late.db")
	ctx := context.Background()
	const sessionID = "sess-late"

	release := make(chan struct{})
	inst := newE2EInstance(t, dbPath, 20*time.Millisecond, lateAsk(release, true))
	defer inst.close()
	createSession(t, inst.db, sessionID)

	resp, err := inst.agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChain(),
		ChainRef:   "e2e-chain.json",
	})
	require.NoError(t, err, "a suspension is a typed outcome, not an error")
	require.Equal(t, agentservice.StopSuspended, resp.StopReason, "the park window must elapse before the late press lands")
	require.Equal(t, "call-w1", resp.SuspendedApprovalID)
	require.Zero(t, inst.inner.execCount(), "the gated tool must not run before the verdict lands")

	row, err := inst.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State)

	close(release)

	require.Eventually(t, func() bool {
		return inst.inner.execCount() == 1
	}, 2*time.Second, 10*time.Millisecond,
		"a late in-band verdict must still resume the checkpointed run and execute the gated tool — "+
			"this is the live 'pressed y, stuck forever' repro")

	require.Eventually(t, func() bool {
		kinds := inst.sink.kinds()
		return len(kinds) > 0 && kinds[len(kinds)-1] == taskengine.TaskEventChainCompleted
	}, 2*time.Second, 10*time.Millisecond, "the turn must complete visibly — a turn may never terminate silently")

	row, err = inst.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State)

	// the resume runs detached in its own goroutine, so "chain completed" can observably land before the checkpoint row is actually deleted; poll, and do it before inst.close() so the background resume never races the DB close
	require.Eventually(t, func() bool {
		_, err := inst.store.GetChainCheckpoint(ctx, "call-w1")
		return err != nil
	}, 2*time.Second, 10*time.Millisecond, "the checkpoint must be consumed by the resume, not left stranded")

	msgs := loadSessionMessages(t, inst.db, sessionID)
	var toolResults []string
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == "call-w1" {
			toolResults = append(toolResults, m.Content)
		}
	}
	require.Len(t, toolResults, 1)
	require.Contains(t, toolResults[0], "executed:write")
}

// TestSystem_S6Gate_LateVerdictThroughTheSameAskChannel_DenyAlsoResumes pins
// that a late deny (not just a late approve) resumes and completes with the
// standard deny semantics — the fix must not special-case "true".
func TestSystem_S6Gate_LateVerdictThroughTheSameAskChannel_DenyAlsoResumes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "s6-gate-late-deny.db")
	ctx := context.Background()
	const sessionID = "sess-late-deny"

	release := make(chan struct{})
	inst := newE2EInstance(t, dbPath, 20*time.Millisecond, lateAsk(release, false))
	defer inst.close()
	createSession(t, inst.db, sessionID)

	resp, err := inst.agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChain(),
		ChainRef:   "e2e-chain.json",
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)

	close(release)

	require.Eventually(t, func() bool {
		kinds := inst.sink.kinds()
		return len(kinds) > 0 && kinds[len(kinds)-1] == taskengine.TaskEventChainCompleted
	}, 2*time.Second, 10*time.Millisecond, "a late denial must also complete the turn visibly")

	require.Zero(t, inst.inner.execCount(), "a denied call must never execute")
	row, err := inst.store.GetHITLApproval(ctx, "call-w1")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalDenied, row.State)

	// the detached resume must fully finish before the deferred inst.close() runs, or its tail end races the DB close
	require.Eventually(t, func() bool {
		_, err := inst.store.GetChainCheckpoint(ctx, "call-w1")
		return err != nil
	}, 2*time.Second, 10*time.Millisecond, "the checkpoint must be consumed by the resume, not left stranded")
}

func e2eChainWithFollowup() *taskengine.TaskChainDefinition {
	chain := e2eChain()
	chain.ID = "chain.e2e.followup"
	chain.Tasks[0].Transition = taskengine.TaskTransition{
		Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: "followup"}},
	}
	chain.Tasks = append(chain.Tasks, taskengine.TaskDefinition{
		ID:            "followup",
		Handler:       taskengine.HandleChatCompletion,
		ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "m"},
		Transition: taskengine.TaskTransition{
			Branches: []taskengine.TransitionBranch{{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd}},
		},
	})
	return chain
}

// TestSystem_S6Gate_ContinuationModelError_SurfacesAsChainFailure_NeverSilent pins that a follow-up model call failing after an in-window approval surfaces as a real error, with chain_failed as the sink's last event, never a silent success.
func TestSystem_S6Gate_ContinuationModelError_SurfacesAsChainFailure_NeverSilent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "s6-gate-followup-fail.db")
	ctx := context.Background()
	const sessionID = "sess-followup-fail"

	alwaysApproveInline := func(context.Context, hitlservice.ApprovalRequest) (bool, error) { return true, nil }
	inst := newE2EInstance(t, dbPath, time.Minute, alwaysApproveInline)
	defer inst.close()
	createSession(t, inst.db, sessionID)

	resp, err := inst.agent.Prompt(ctx, agentservice.PromptRequest{
		SessionID:  sessionID,
		InputValue: e2eInput(),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      e2eChainWithFollowup(),
		ChainRef:   "e2e-chain-followup.json",
	})

	require.Error(t, err, "a continuation model failure must surface as a real error, never a silent success")
	require.Equal(t, []string{"write"}, inst.inner.execs, "the approved tool call must still have run before the follow-up failed")
	if resp != nil {
		require.NotEqual(t, agentservice.StopSuspended, resp.StopReason, "this run never checkpoints — the verdict lands inside the window")
	}

	kinds := inst.sink.kinds()
	require.NotEmpty(t, kinds)
	require.Equal(t, taskengine.TaskEventChainFailed, kinds[len(kinds)-1],
		"the follow-up model failure must be visible on the event stream a UI subscribes to, not swallowed")
}
