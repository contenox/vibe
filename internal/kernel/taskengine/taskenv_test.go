package taskengine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

type cancelAwareExecutor struct{}

func (cancelAwareExecutor) TaskExec(
	ctx context.Context,
	_ time.Time,
	_ int,
	_ *taskengine.ChainContext,
	_ *taskengine.TaskDefinition,
	_ any,
	_ taskengine.DataType,
) (any, taskengine.DataType, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, taskengine.DataTypeAny, "", err
	}
	return nil, taskengine.DataTypeAny, "", errors.New("task context was not canceled")
}

type calledTask struct {
	task     string
	input    any
	dataType taskengine.DataType
}

type scriptedExecutor struct {
	calls       []calledTask
	outputs     []any
	outputTypes []taskengine.DataType
	transitions []string
	errors      []error
}

func (m *scriptedExecutor) TaskExec(
	_ context.Context,
	_ time.Time,
	_ int,
	_ *taskengine.ChainContext,
	currentTask *taskengine.TaskDefinition,
	input any,
	dataType taskengine.DataType,
) (any, taskengine.DataType, string, error) {
	m.calls = append(m.calls, calledTask{
		task:     currentTask.ID,
		input:    input,
		dataType: dataType,
	})

	output := any(nil)
	if len(m.outputs) > 0 {
		output = m.outputs[0]
		m.outputs = m.outputs[1:]
	}

	outputType := taskengine.DataTypeAny
	if len(m.outputTypes) > 0 {
		outputType = m.outputTypes[0]
		m.outputTypes = m.outputTypes[1:]
	}

	transition := ""
	if len(m.transitions) > 0 {
		transition = m.transitions[0]
		m.transitions = m.transitions[1:]
	}

	err := error(nil)
	if len(m.errors) > 0 {
		err = m.errors[0]
		m.errors = m.errors[1:]
	}

	return output, outputType, transition, err
}

func TestUnit_SimpleEnv_ExecEnv_SingleTask(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "42",
		MockTransitionValue: "42",
		MockError:           nil,
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(t.Context(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `What is {{.input}}?`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: "equals",
							When:     "42",
							Goto:     taskengine.TermEnd,
						},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "6 * 7", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "42", result)
}

func TestUnit_SimpleEnv_ExecEnv_ErrorTransitionPreservesTaskInputForNextTask(t *testing.T) {
	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{{Role: "user", Content: "diagnostic"}},
	}
	exec := &scriptedExecutor{
		outputs:     []any{nil, chainInput},
		outputTypes: []taskengine.DataType{taskengine.DataTypeAny, taskengine.DataTypeChatHistory},
		errors:      []error{errors.New("classify failed"), nil},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "classify_request",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					OnFailure: "acp_chat",
				},
			},
			{
				ID:      "acp_chat",
				Handler: taskengine.HandleChatCompletion,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, chainInput, taskengine.DataTypeAny)
	require.NoError(t, err)
	require.Len(t, exec.calls, 2, "expect failed step and on_failure step to run")
	require.Equal(t, "acp_chat", exec.calls[1].task)
	require.Equal(t, chainInput, exec.calls[1].input)
	require.Equal(t, taskengine.DataTypeChatHistory, exec.calls[1].dataType)
}

func TestUnit_SimpleEnv_ExecEnv_CapsInputForFailureSummaryTask(t *testing.T) {
	longContent := strings.Repeat("oversized ", 200)
	chainInput := taskengine.ChatHistory{
		Messages:     []taskengine.Message{{Role: "user", Content: longContent}},
		InputTokens:  999,
		OutputTokens: 1,
	}
	exec := &scriptedExecutor{
		outputs:     []any{nil, "summary"},
		outputTypes: []taskengine.DataType{taskengine.DataTypeAny, taskengine.DataTypeString},
		errors:      []error{errors.New("context overflow"), nil},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "main",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					OnFailure: "summarise_failure",
				},
			},
			{
				ID:            "summarise_failure",
				Handler:       taskengine.HandleNoop,
				InputMaxBytes: 64,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, chainInput, taskengine.DataTypeChatHistory)
	require.NoError(t, err)
	require.Len(t, exec.calls, 2)
	require.Equal(t, "summarise_failure", exec.calls[1].task)
	require.Equal(t, taskengine.DataTypeChatHistory, exec.calls[1].dataType)
	got := exec.calls[1].input.(taskengine.ChatHistory)
	require.Zero(t, got.InputTokens, "truncated chat input must be recounted")
	require.Zero(t, got.OutputTokens, "truncated chat input must be recounted")
	require.Len(t, got.Messages, 1)
	require.Less(t, len(got.Messages[0].Content), len(longContent))
	require.Contains(t, got.Messages[0].Content, "truncated original_bytes=")
}

// A failure handler whose routed context is whittled to nothing usable must
// still receive the failure as a non-empty user message it can report.
func TestUnit_SimpleEnv_ExecEnv_FilteredEmptyFailureContextStillCarriesRootError(t *testing.T) {
	rootErr := errors.New("stream failed (provider=vertex-google, model=gemini-3.6-flash): vertex API returned non-200 status for stream: 404")
	chainInput := taskengine.ChatHistory{
		Messages: []taskengine.Message{
			{Role: "user", Content: "run the workflow"},
			{Role: "assistant", CallTools: []taskengine.ToolCall{{ID: "call-1"}}},
			{Role: "tool", ToolCallID: "call-1", Content: strings.Repeat("tool output ", 40)},
		},
	}
	exec := &scriptedExecutor{
		outputs:     []any{nil, "summary"},
		outputTypes: []taskengine.DataType{taskengine.DataTypeAny, taskengine.DataTypeString},
		errors:      []error{rootErr, nil},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "main",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					OnFailure: "summarise_failure",
				},
			},
			{
				ID:            "summarise_failure",
				Handler:       taskengine.HandleNoop,
				InputMaxBytes: 64,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, chainInput, taskengine.DataTypeChatHistory)
	require.NoError(t, err, "the failure handler must be able to report instead of dying of its own prompt")
	require.Equal(t, "summary", result)
	require.Len(t, exec.calls, 2)
	require.Equal(t, "summarise_failure", exec.calls[1].task)
	got := exec.calls[1].input.(taskengine.ChatHistory)
	require.NotEmpty(t, got.Messages)
	last := got.Messages[len(got.Messages)-1]
	require.Equal(t, "user", last.Role)
	require.Contains(t, last.Content, rootErr.Error(), "the root failure must land as the handler's context")
	require.Contains(t, last.Content, "main", "the notice names the failed task")
}

func TestUnit_SimpleEnv_ExecEnv_FailureHandlerFailureLeadsWithRootError(t *testing.T) {
	rootErr := errors.New("chat stream failed: stream failed (provider=vertex-google, model=gemini-3.6-flash): 404")
	handlerErr := errors.New("summariser also down")
	exec := &scriptedExecutor{
		outputs:     []any{nil, nil},
		outputTypes: []taskengine.DataType{taskengine.DataTypeAny, taskengine.DataTypeAny},
		errors:      []error{rootErr, handlerErr},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "main",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					OnFailure: "summarise_failure",
				},
			},
			{
				ID:      "summarise_failure",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "input", taskengine.DataTypeString)
	require.Error(t, err)
	require.EqualError(t, err, "task main failed after 0 retries: task main: "+rootErr.Error()+" (on_failure handler also failed: task summarise_failure: "+handlerErr.Error()+")")
	require.ErrorIs(t, err, rootErr, "root cause must stay reachable via errors.Is")
	require.ErrorIs(t, err, handlerErr, "handler failure must stay reachable via errors.Is")
}

func TestUnit_SimpleEnv_ExecEnv_RecoveredFailureDoesNotTaintLaterFailure(t *testing.T) {
	laterErr := errors.New("later failure")
	exec := &scriptedExecutor{
		outputs:     []any{nil, "recovered", nil},
		outputTypes: []taskengine.DataType{taskengine.DataTypeAny, taskengine.DataTypeString, taskengine.DataTypeAny},
		errors:      []error{errors.New("recovered failure"), nil, laterErr},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, exec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "main",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					OnFailure: "recover",
				},
			},
			{
				ID:      "recover",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "later"},
					},
				},
			},
			{
				ID:      "later",
				Handler: taskengine.HandleNoop,
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "input", taskengine.DataTypeString)
	require.Error(t, err)
	require.EqualError(t, err, "task later failed after 0 retries: task later: "+laterErr.Error())
}

func TestUnit_SimpleEnv_ExecEnv_UsesParentCancellation(t *testing.T) {
	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, cancelAwareExecutor{}, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `What is {{.input}}?`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err = env.ExecEnv(ctx, chain, "6 * 7", taskengine.DataTypeString)
	require.Error(t, err)
	require.Contains(t, err.Error(), context.Canceled.Error())
}

func TestUnit_SimpleEnv_ExecEnv_FailsAfterRetries(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockError: errors.New("permanent failure"),
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `Broken task`,
				RetryOnFailure: 1,
				Transition:     taskengine.TaskTransition{},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "", taskengine.DataTypeString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed after 1 retries")
}

func TestUnit_SimpleEnv_ExecEnv_TransitionsToNextTask(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "intermediate",
		MockTransitionValue: "continue",
		MockError:           nil,
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: "continue", Goto: "task2"},
					},
				},
			},
			{
				ID:      "task2",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: "continue", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "test", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "intermediate", result)
}

func TestUnit_SimpleEnv_ExecEnv_ErrorTransition(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		ErrorSequence:       []error{errors.New("first failure"), nil},
		MockOutput:          "error recovered",
		MockTransitionValue: "recovered",
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `fail`,
				Transition: taskengine.TaskTransition{
					OnFailure: "task2",
				},
			},
			{
				ID:             "task2",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `recover`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "equals", When: "recovered", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "oops", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "error recovered", result)
}

func TestUnit_SimpleEnv_ExecEnv_PrintTemplate(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "printed-value",
		MockTransitionValue: "printed-value",
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `hi {{.input}}`,
				Print:          `Output: {{.task1}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "equals", When: "printed-value", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "user", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "printed-value", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_OriginalInput(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "processed: hello",
		MockTransitionValue: "processed: hello",
		MockError:           nil,
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				InputVar:       "input",
				PromptTemplate: `Process this: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "hello", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "processed: hello", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_PreviousTaskOutput(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence:          []any{"42", "processed: 42"},
		MockTransitionValueSequence: []string{"42", "processed: 42"},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "transform",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `Convert to number: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: "process"},
					},
				},
			},
			{
				ID:             "process",
				Handler:        taskengine.HandleNoop,
				InputVar:       "transform",
				PromptTemplate: `Process the number: {{.transform}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "forty-two", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "processed: 42", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_BranchRouting(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence:          []any{8, "user message stored"},
		MockTransitionValueSequence: []string{"store", "user message stored"},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "moderate",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `Rate safety of: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpEquals, When: "store", Goto: "store"},
						{Operator: "default", Goto: "reject"},
					},
				},
			},
			{
				ID:       "store",
				Handler:  taskengine.HandleTools,
				InputVar: "input",
				Tools: &taskengine.ToolsCall{
					Name: "store_message",
				},
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
			{
				ID:             "reject",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `Rejected: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "safe message", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "user message stored", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_InvalidVariable(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				InputVar:       "nonexistent",
				PromptTemplate: `Should fail`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "test", taskengine.DataTypeString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input variable")
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_DefaultBehavior(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence:          []any{"first", "second"},
		MockTransitionValueSequence: []string{"first", "second"},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `First: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: "task2"},
					},
				},
			},
			{
				ID:             "task2",
				Handler:        taskengine.HandleNoop,
				PromptTemplate: `Second: {{.task1}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "input", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "second", result)
}
