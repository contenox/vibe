package taskengine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/stretchr/testify/require"
)

type recordedOp struct {
	op        string
	errs      int
	changes   int
	changeLog []recordedChange
	ended     int
}

type recordedChange struct {
	id   string
	data any
}

type recordingTracker struct {
	mu  sync.Mutex
	ops []*recordedOp
}

func (rt *recordingTracker) Start(_ context.Context, op, _ string, _ ...any) (func(error), func(string, any), func()) {
	rt.mu.Lock()
	o := &recordedOp{op: op}
	rt.ops = append(rt.ops, o)
	rt.mu.Unlock()
	return func(error) { rt.mu.Lock(); o.errs++; rt.mu.Unlock() },
		func(id string, data any) {
			rt.mu.Lock()
			o.changes++
			o.changeLog = append(o.changeLog, recordedChange{id: id, data: data})
			rt.mu.Unlock()
		},
		func() { rt.mu.Lock(); o.ended++; rt.mu.Unlock() }
}

func (rt *recordingTracker) find(op string) *recordedOp {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, o := range rt.ops {
		if o.op == op {
			return o
		}
	}
	return nil
}

func (rt *recordingTracker) changesFor(op string) []recordedChange {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, o := range rt.ops {
		if o.op == op {
			return append([]recordedChange(nil), o.changeLog...)
		}
	}
	return nil
}

func execToolCallsHarness(t *testing.T, toolErr error) (any, taskengine.DataType, error, *recordingTracker) {
	t.Helper()

	repo := &tools.MockToolsRepo{
		ResponseMap:     map[string]tools.ToolsResponse{},
		DefaultResponse: tools.ToolsResponse{Output: "ignored"},
		ErrorSequence:   []error{toolErr},
	}

	rt := &recordingTracker{}
	exec, err := taskengine.NewExec(context.Background(), &mockModelRepo{}, repo, rt)
	require.NoError(t, err)

	chainCtx := &taskengine.ChainContext{
		Tools: map[string]taskengine.ToolWithResolution{
			"cancel_tool": {
				Tool:      taskengine.Tool{Type: "function", Function: taskengine.FunctionTool{Name: "cancel_tool"}},
				ToolsName: "cancel_tool",
			},
		},
	}

	history := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{
				Role: "assistant",
				CallTools: []taskengine.ToolCall{
					{ID: "c1", Type: "function", Function: taskengine.FunctionCall{Name: "cancel_tool", Arguments: "{}"}},
				},
			},
		},
	}

	currentTask := &taskengine.TaskDefinition{ID: "exec", Handler: taskengine.HandleExecuteToolCalls}

	out, dt, _, err := exec.TaskExec(
		context.Background(), time.Now().UTC(), 4000,
		chainCtx, currentTask, history, taskengine.DataTypeChatHistory)
	return out, dt, err, rt
}

func TestUnit_ExecuteToolCalls_CancellationPropagatesNotSwallowed(t *testing.T) {
	_, _, err, rt := execToolCallsHarness(t, context.Canceled)
	require.Error(t, err, "a cancelled tool must abort the turn, not be soft-swallowed into a tool message")
	require.ErrorIs(t, err, context.Canceled,
		"the context.Canceled chain must stay intact so InferStopReason yields StopCancelled")

	span := rt.find("tool_call")
	require.NotNil(t, span, "the tool_call activity span must still be opened on cancel")
	require.Equal(t, 1, span.errs, "a cancelled tool call must be reported via reportErr, not silently dropped from telemetry")
	require.Equal(t, 0, span.changes, "cancel is a failure outcome: reportChange must not be called")
	require.Equal(t, 1, span.ended, "the tracker span must be ended exactly once")
}

func TestUnit_ExecuteToolCalls_OrdinaryToolErrorStaysSoft(t *testing.T) {
	out, _, err, rt := execToolCallsHarness(t, errors.New("file not found"))
	require.NoError(t, err, "recoverable tool errors must remain soft so the model can react")

	hist, ok := out.(taskengine.ChatHistory)
	require.True(t, ok)
	last := hist.Messages[len(hist.Messages)-1]
	require.Equal(t, "tool", last.Role)
	require.Contains(t, last.Content, "execution failed")

	span := rt.find("tool_call")
	require.NotNil(t, span)
	require.Equal(t, 1, span.errs, "an ordinary tool error is still a failed tool_call span (unchanged behavior)")
	require.Equal(t, 1, span.ended)
}
