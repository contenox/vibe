package taskengine_test

// Engine-side suspend/resume contract: a gated tool call that parks past the
// fast window suspends the run (checkpoint persisted before release, chain_suspended as
// the segment terminal, no stub results over the pending calls), and a resume
// re-enters through the execute_tool_calls repair path, executes exactly the
// unanswered calls, and completes with no duplicated work.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedToolsRepo executes "gate.read" immediately and, while gated, answers
// "gate.write" with the wrapper's third outcome (ApprovalPendingError keyed
// by the engine-minted call ID it finds on the context — the same ID the
// production HITL wrapper uses as approval/checkpoint key).
type gatedToolsRepo struct {
	gated bool
	execs []string // "<toolName>:<callID>" in execution order
}

func (g *gatedToolsRepo) Exec(ctx context.Context, _ time.Time, _ any, _ bool, args *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	callID, _ := ctx.Value(taskengine.ContextKeyToolCallID).(string)
	g.execs = append(g.execs, args.ToolName+":"+callID)
	if g.gated && args.ToolName == "write" {
		return nil, taskengine.DataTypeAny, &taskengine.ApprovalPendingError{ApprovalID: callID, ToolName: "gate.write"}
	}
	return "ok:" + args.ToolName, taskengine.DataTypeString, nil
}

func (g *gatedToolsRepo) Supports(context.Context) ([]string, error) { return []string{"gate"}, nil }

func (g *gatedToolsRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

func (g *gatedToolsRepo) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	if name != "gate" {
		return nil, taskengine.ErrToolsNotFound
	}
	return []taskengine.Tool{
		{Type: "function", Function: taskengine.FunctionTool{Name: "read"}},
		{Type: "function", Function: taskengine.FunctionTool{Name: "write"}},
	}, nil
}

// captureCheckpointSaver records what the engine persists on suspension.
type captureCheckpointSaver struct {
	saved []*taskengine.Checkpoint
	err   error
}

func (s *captureCheckpointSaver) SaveCheckpoint(_ context.Context, cp *taskengine.Checkpoint) error {
	if s.err != nil {
		return s.err
	}
	s.saved = append(s.saved, cp)
	return nil
}

func suspendTestChain() *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{
		ID: "chain.suspend",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:            "exec",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: &taskengine.LLMExecutionConfig{Model: "m", Tools: []string{"gate"}},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}
}

func suspendTestHistory() taskengine.ChatHistory {
	return taskengine.ChatHistory{Messages: []taskengine.Message{
		{ID: "m-user", Role: "user", Content: "do things", Timestamp: time.Now().UTC()},
		{ID: "m-asst", Role: "assistant", Timestamp: time.Now().UTC(), CallTools: []taskengine.ToolCall{
			{ID: "c1", Type: "function", Function: taskengine.FunctionCall{Name: "gate.read", Arguments: `{}`}},
			{ID: "c2", Type: "function", Function: taskengine.FunctionCall{Name: "gate.write", Arguments: `{"path":"x"}`}},
			{ID: "c3", Type: "function", Function: taskengine.FunctionCall{Name: "gate.read", Arguments: `{"p":2}`}},
		}},
	}}
}

func newSuspendEnv(t *testing.T, repo *gatedToolsRepo, sink taskengine.TaskEventSink) taskengine.EnvExecutor {
	t.Helper()
	cctx := taskengine.WithTaskEventSink(context.Background(), sink)
	exec, err := taskengine.NewExec(cctx, &mockModelRepo{}, repo, libtracker.NoopTracker{})
	require.NoError(t, err)
	env, err := taskengine.NewEnv(cctx, libtracker.NoopTracker{}, exec, taskengine.NewSimpleInspector(), repo)
	require.NoError(t, err)
	return env
}

func toolResultCounts(msgs []taskengine.Message) map[string]int {
	counts := map[string]int{}
	for _, m := range msgs {
		if m.Role == "tool" {
			counts[m.ToolCallID]++
		}
	}
	return counts
}

func TestContract_SuspendThenResume_ExecutesPendingWithoutDuplication(t *testing.T) {
	repo := &gatedToolsRepo{gated: true}
	sink := &captureTaskEventSink{}
	env := newSuspendEnv(t, repo, sink)
	saver := &captureCheckpointSaver{}

	ctx := taskengine.WithCheckpointSaver(context.Background(), saver)
	ctx = taskengine.WithTemplateVars(ctx, map[string]string{"chain": "chain.suspend"})
	ctx = taskengine.WithRuntimeToolsAllowlist(ctx, []string{"gate"})
	ctx = context.WithValue(ctx, libtracker.ContextKeyRequestID, "req-suspend")

	out, outType, _, err := env.ExecEnv(ctx, suspendTestChain(), suspendTestHistory(), taskengine.DataTypeChatHistory)

	// The suspension terminal
	var susp *taskengine.ChainSuspendedError
	require.ErrorAs(t, err, &susp, "a parked approval must suspend the run, not fail it")
	require.Equal(t, "c2", susp.ApprovalID, "the approval ID IS the engine-minted call ID")
	require.Equal(t, taskengine.EventScope{Chain: "chain.suspend", Task: "exec", ToolCall: "c2"}, susp.Scope)

	// The partial history is returned: c1's real result present, no stub
	// results for the pending c2/c3 (the checkpointed transcript must end
	// with them unanswered for the repair path to re-enter).
	require.Equal(t, taskengine.DataTypeChatHistory, outType)
	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, map[string]int{"c1": 1}, toolResultCounts(hist.Messages))

	// The checkpoint (persisted before release)
	require.Len(t, saver.saved, 1)
	cp := saver.saved[0]
	assert.Equal(t, "c2", cp.ApprovalID)
	assert.Equal(t, "exec", cp.TaskID)
	assert.Equal(t, 0, cp.RetryIndex)
	assert.Equal(t, susp.Scope, cp.Scope)
	assert.Equal(t, []taskengine.PendingToolCall{
		{CallID: "c2", Name: "gate.write", Arguments: `{"path":"x"}`},
		{CallID: "c3", Name: "gate.read", Arguments: `{"p":2}`},
	}, cp.PendingCalls, "composite batch: the awaiting call first, then the unstarted rest")
	assert.Equal(t, map[string]int{"c1": 1}, toolResultCounts(cp.History.Messages))
	assert.Equal(t, map[string]string{"chain": "chain.suspend"}, cp.TemplateVars)
	assert.True(t, cp.HasToolsAllowlist)
	assert.Equal(t, []string{"gate"}, cp.ToolsAllowlist)
	assert.Equal(t, "req-suspend", cp.RequestID)
	require.NotNil(t, cp.Chain)
	assert.Equal(t, "chain.suspend", cp.Chain.ID)

	// The event contract
	var kinds []taskengine.TaskEventKind
	for _, ev := range sink.events {
		kinds = append(kinds, ev.Kind)
	}
	require.Equal(t, []taskengine.TaskEventKind{
		taskengine.TaskEventChainStarted,
		taskengine.TaskEventStepStarted,
		taskengine.TaskEventToolCallPending, // c1
		taskengine.TaskEventToolCall,        // c1
		taskengine.TaskEventToolCallPending, // c2 — left unbracketed by design
		taskengine.TaskEventChainSuspended,  // segment terminal, after the checkpoint write
	}, kinds)
	suspEv := sink.events[len(sink.events)-1]
	assert.Equal(t, "c2", suspEv.ApprovalID)
	assert.Equal(t, susp.Scope, suspEv.Scope)

	// Round-trip through the wire format, as the real resume path does
	raw, err := taskengine.MarshalCheckpoint(cp)
	require.NoError(t, err)
	restored, err := taskengine.UnmarshalCheckpoint(raw)
	require.NoError(t, err)

	// Resume: verdict approved, gate released
	repo.gated = false
	repo.execs = nil
	resumeSink := &captureTaskEventSink{}
	resumeEnv := newSuspendEnv(t, repo, resumeSink)
	rctx := taskengine.WithResumeCheckpoint(context.Background(), restored)
	rctx = taskengine.WithApprovalVerdicts(rctx, map[string]bool{"c2": true})
	rctx = taskengine.WithCheckpointSaver(rctx, saver)

	rout, routType, _, rerr := resumeEnv.ExecEnv(rctx, suspendTestChain(), restored.History, taskengine.DataTypeChatHistory)
	require.NoError(t, rerr)
	require.Equal(t, taskengine.DataTypeChatHistory, routType)

	// Only the unanswered calls executed — c1 was not re-run.
	require.Equal(t, []string{"write:c2", "read:c3"}, repo.execs)

	final, ok := rout.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, map[string]int{"c1": 1, "c2": 1, "c3": 1}, toolResultCounts(final.Messages),
		"every call answered exactly once across suspend + resume")

	// Resumed segment closes with chain_completed and re-emits the pending/
	// call pair for the calls it executed.
	require.Equal(t, taskengine.TaskEventChainCompleted, resumeSink.events[len(resumeSink.events)-1].Kind)
}

func TestContract_Suspend_NoSaverFailsTeaching(t *testing.T) {
	repo := &gatedToolsRepo{gated: true}
	env := newSuspendEnv(t, repo, &captureTaskEventSink{})

	// No CheckpointSaver on the context: the run must fail with a teaching
	// error, never suspend into nowhere or hang.
	_, _, _, err := env.ExecEnv(context.Background(), suspendTestChain(), suspendTestHistory(), taskengine.DataTypeChatHistory)
	require.Error(t, err)
	var susp *taskengine.ChainSuspendedError
	require.False(t, errors.As(err, &susp), "without a saver there is no durable suspension to report")
	require.Contains(t, err.Error(), "checkpoint saver")
	require.Contains(t, err.Error(), "WithCheckpointSaver")
}

func TestContract_Suspend_SaverFailureFailsRun(t *testing.T) {
	repo := &gatedToolsRepo{gated: true}
	env := newSuspendEnv(t, repo, &captureTaskEventSink{})
	saver := &captureCheckpointSaver{err: errors.New("disk full")}

	ctx := taskengine.WithCheckpointSaver(context.Background(), saver)
	_, _, _, err := env.ExecEnv(ctx, suspendTestChain(), suspendTestHistory(), taskengine.DataTypeChatHistory)
	require.Error(t, err)
	var susp *taskengine.ChainSuspendedError
	require.False(t, errors.As(err, &susp))
	require.Contains(t, err.Error(), "disk full")
}

// TestContract_ResumeAfterDeny: a denied verdict resumes the run with the
// existing deny semantics — here modeled at the engine seam: the wrapper
// (exercised in localtools' own tests) turns the injected false verdict into
// its deny-message result, so from the engine's perspective the call simply
// yields a result and the chain completes.
func TestContract_ResumeAfterDeny_CompletesWithDenyResult(t *testing.T) {
	repo := &gatedToolsRepo{gated: true}
	sink := &captureTaskEventSink{}
	env := newSuspendEnv(t, repo, sink)
	saver := &captureCheckpointSaver{}

	ctx := taskengine.WithCheckpointSaver(context.Background(), saver)
	_, _, _, err := env.ExecEnv(ctx, suspendTestChain(), suspendTestHistory(), taskengine.DataTypeChatHistory)
	var susp *taskengine.ChainSuspendedError
	require.ErrorAs(t, err, &susp)
	require.Len(t, saver.saved, 1)

	// Deny: the wrapper (simulated by the un-gated repo returning a plain
	// result) answers the pending call; the chain must run to completion and
	// answer every call of the batch.
	repo.gated = false
	rctx := taskengine.WithResumeCheckpoint(context.Background(), saver.saved[0])
	rctx = taskengine.WithApprovalVerdicts(rctx, map[string]bool{"c2": false})
	rout, _, _, rerr := env.ExecEnv(rctx, suspendTestChain(), saver.saved[0].History, taskengine.DataTypeChatHistory)
	require.NoError(t, rerr)
	final := rout.(taskengine.ChatHistory)
	require.Equal(t, map[string]int{"c1": 1, "c2": 1, "c3": 1}, toolResultCounts(final.Messages))
}
