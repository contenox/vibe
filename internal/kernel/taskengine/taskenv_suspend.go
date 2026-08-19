package taskengine

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/contenox/libtracker"
)

// checkpointSaveGrace bounds the detached checkpoint write below, so a
// shutdown cannot hang on an unreachable store.
const checkpointSaveGrace = 15 * time.Second

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
			fmt.Errorf("task %s: approval %s is pending but no checkpoint saver is installed on this run, so it cannot suspend durably; install one via taskengine.WithCheckpointSaver (agentservice does), or let the run block on the ask instead of detaching it (taskengine.WithDetachedAsks)", currentTask.ID, pendErr.ApprovalID)
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

	// A suspension is often triggered by this process leaving (ctx cancelled,
	// shutdown): the write that makes the run resumable elsewhere must outlive
	// the cancellation that caused it, or nothing is left behind to resume.
	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), checkpointSaveGrace)
	defer cancelSave()
	if err := saver.SaveCheckpoint(saveCtx, cp); err != nil {
		return nil, DataTypeAny, stack.GetExecutionHistory(),
			fmt.Errorf("task %s: persisting the suspension checkpoint for approval %s failed (the run cannot suspend durably): %w", currentTask.ID, pendErr.ApprovalID, err)
	}

	return history, DataTypeChatHistory, stack.GetExecutionHistory(), &ChainSuspendedError{
		ApprovalID: pendErr.ApprovalID,
		Scope:      scope,
	}
}
