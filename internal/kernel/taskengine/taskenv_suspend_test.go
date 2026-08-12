package taskengine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gatedToolsRepo struct {
	gated bool
	execs []string
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

	var susp *taskengine.ChainSuspendedError
	require.ErrorAs(t, err, &susp, "a parked approval must suspend the run, not fail it")
	require.Equal(t, "c2", susp.ApprovalID, "the approval ID IS the engine-minted call ID")
	require.Equal(t, taskengine.EventScope{Chain: "chain.suspend", Task: "exec", ToolCall: "c2"}, susp.Scope)

	// No stub results for pending c2/c3: the checkpointed transcript must end with them unanswered for the repair path to re-enter.
	require.Equal(t, taskengine.DataTypeChatHistory, outType)
	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, map[string]int{"c1": 1}, toolResultCounts(hist.Messages))

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

	var kinds []taskengine.TaskEventKind
	for _, ev := range sink.events {
		kinds = append(kinds, ev.Kind)
	}
	require.Equal(t, []taskengine.TaskEventKind{
		taskengine.TaskEventChainStarted,
		taskengine.TaskEventStepStarted,
		taskengine.TaskEventToolCallPending,
		taskengine.TaskEventToolCall,
		taskengine.TaskEventToolCallPending, // c2, left unbracketed by design
		taskengine.TaskEventChainSuspended,  // after the checkpoint write
	}, kinds)
	suspEv := sink.events[len(sink.events)-1]
	assert.Equal(t, "c2", suspEv.ApprovalID)
	assert.Equal(t, susp.Scope, suspEv.Scope)

	raw, err := taskengine.MarshalCheckpoint(cp)
	require.NoError(t, err)
	restored, err := taskengine.UnmarshalCheckpoint(raw)
	require.NoError(t, err)

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

	require.Equal(t, []string{"write:c2", "read:c3"}, repo.execs)

	final, ok := rout.(taskengine.ChatHistory)
	require.True(t, ok)
	require.Equal(t, map[string]int{"c1": 1, "c2": 1, "c3": 1}, toolResultCounts(final.Messages),
		"every call answered exactly once across suspend + resume")

	require.Equal(t, taskengine.TaskEventChainCompleted, resumeSink.events[len(resumeSink.events)-1].Kind)
}

func TestContract_Suspend_NoSaverFailsTeaching(t *testing.T) {
	repo := &gatedToolsRepo{gated: true}
	env := newSuspendEnv(t, repo, &captureTaskEventSink{})

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

// TestContract_ResumeAfterDeny_CompletesWithDenyResult verifies a denied verdict resumes the run and completes with the wrapper's deny-message result.
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

	repo.gated = false
	rctx := taskengine.WithResumeCheckpoint(context.Background(), saver.saved[0])
	rctx = taskengine.WithApprovalVerdicts(rctx, map[string]bool{"c2": false})
	rout, _, _, rerr := env.ExecEnv(rctx, suspendTestChain(), saver.saved[0].History, taskengine.DataTypeChatHistory)
	require.NoError(t, rerr)
	final := rout.(taskengine.ChatHistory)
	require.Equal(t, map[string]int{"c1": 1, "c2": 1, "c3": 1}, toolResultCounts(final.Messages))
}
