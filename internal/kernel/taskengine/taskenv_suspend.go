package taskengine

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/libtracker"
)

// suspendRun turns an in-flight ApprovalPendingError into a durable
// suspension: it assembles the checkpoint from the live run state, persists
// it through the context-installed CheckpointSaver, and returns the partial
// history with a typed ChainSuspendedError. The deferred chain-event
// publisher in ExecEnv sees that error and emits chain_suspended (after the
// checkpoint is durable) instead of chain_failed.
//
// Ordering invariant: the durable approval row was created by the HITL
// wrapper BEFORE it parked (row first); the checkpoint is persisted here
// (checkpoint second); only then does the run release (release third). A
// missing or failing saver fails the run with a teaching error instead of
// suspending — a suspension without a checkpoint is a lost run, and the
// pending approval row is then closed out by hitlservice's expiry sweeper.
func (env SimpleEnv) suspendRun(
	ctx context.Context,
	stack StackTrace,
	chain *TaskChainDefinition,
	currentTask *TaskDefinition,
	retry int,
	vars map[string]any,
	varTypes map[string]DataType,
	edgeCounts map[string]int,
	output any,
	pendErr *ApprovalPendingError,
) (any, DataType, []CapturedStateUnit, error) {
	history, ok := output.(ChatHistory)
	if !ok {
		return nil, DataTypeAny, stack.GetExecutionHistory(),
			fmt.Errorf("SEVERBUG: task %s suspended on approval %s but produced %T instead of the chat history a checkpoint requires", currentTask.ID, pendErr.ApprovalID, output)
	}

	saver := checkpointSaverFromContext(ctx)
	if saver == nil {
		return nil, DataTypeAny, stack.GetExecutionHistory(),
			fmt.Errorf("task %s: approval %s is pending but no checkpoint saver is installed on this run, so it cannot suspend durably; install one via taskengine.WithCheckpointSaver (agentservice does) or answer approvals within the fast window", currentTask.ID, pendErr.ApprovalID)
	}

	scope := EventScope{Chain: chain.ID, Task: currentTask.ID, ToolCall: pendErr.ApprovalID}
	cp := &Checkpoint{
		ApprovalID:   pendErr.ApprovalID,
		PendingCalls: unansweredToolCalls(history.Messages),
		Chain:        chain,
		TaskID:       currentTask.ID,
		RetryIndex:   retry,
		Scope:        scope,
		Vars:         vars,
		VarTypes:     varTypes,
		EdgeCounts:   edgeCounts,
		History:      history,
		CreatedAt:    time.Now().UTC(),
	}
	if tv, err := TemplateVarsFromContext(ctx); err == nil {
		cp.TemplateVars = tv
	}
	if allowlist, ok := RuntimeToolsAllowlistFromContext(ctx); ok {
		cp.ToolsAllowlist = allowlist
		cp.HasToolsAllowlist = true
	}
	cp.ContextLength = RequestedContextLengthFromContext(ctx)
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok {
		cp.RequestID = reqID
	}

	if err := saver.SaveCheckpoint(ctx, cp); err != nil {
		return nil, DataTypeAny, stack.GetExecutionHistory(),
			fmt.Errorf("task %s: persisting the suspension checkpoint for approval %s failed (the run cannot suspend durably): %w", currentTask.ID, pendErr.ApprovalID, err)
	}

	return history, DataTypeChatHistory, stack.GetExecutionHistory(), &ChainSuspendedError{
		ApprovalID: pendErr.ApprovalID,
		Scope:      scope,
	}
}
