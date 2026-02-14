package taskengine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
	"github.com/stretchr/testify/require"
)

func TestUnit_SimpleEnv_ExecEnv_SingleTask(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "42",
		MockTransitionValue: "42",
		MockError:           nil,
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(t.Context(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
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

	result, _, _, err := env.ExecEnv(context.Background(), chain, "6 * 7", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "42", result)
}

func TestUnit_SimpleEnv_ExecEnv_FailsAfterRetries(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockError: errors.New("permanent failure"),
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: `Broken task`,
				RetryOnFailure: 1,
				Transition:     taskengine.TaskTransition{},
			},
		},
	}

	_, _, _, err = env.ExecEnv(context.Background(), chain, "", taskengine.DataTypeString)
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
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
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

	result, _, _, err := env.ExecEnv(context.Background(), chain, "test", taskengine.DataTypeString)
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
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: `fail`,
				Transition: taskengine.TaskTransition{
					OnFailure: "task2",
				},
			},
			{
				ID:             "task2",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: `recover`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "equals", When: "recovered", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "oops", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "error recovered", result)
}

func TestUnit_SimpleEnv_ExecEnv_PrintTemplate(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutput:          "printed-value",
		MockTransitionValue: "printed-value",
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
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

	result, _, _, err := env.ExecEnv(context.Background(), chain, "user", taskengine.DataTypeString)
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
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
				InputVar:       "input", // Explicitly use original input
				PromptTemplate: `Process this: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "hello", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "processed: hello", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_PreviousTaskOutput(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence:          []any{"42", "processed: 42"},
		MockTransitionValueSequence: []string{"42", "processed: 42"},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "transform",
				Handler:        taskengine.HandlePromptToInt,
				PromptTemplate: `Convert to number: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: "process"},
					},
				},
			},
			{
				ID:             "process",
				Handler:        taskengine.HandlePromptToString,
				InputVar:       "transform", // Use output from previous task
				PromptTemplate: `Process the number: {{.transform}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "forty-two", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "processed: 42", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_WithModeration(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence:          []any{8, "user message stored"},
		MockTransitionValueSequence: []string{"8", "user message stored"},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "moderate",
				Handler:        taskengine.HandlePromptToInt,
				PromptTemplate: `Rate safety of: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpGreaterThan, When: "5", Goto: "store"},
						{Operator: "default", Goto: "reject"},
					},
				},
			},
			{
				ID:       "store",
				Handler:  taskengine.HandleHook,
				InputVar: "input", // Use original input despite moderation
				Hook: &taskengine.HookCall{
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
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: `Rejected: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "safe message", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "user message stored", result)
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_InvalidVariable(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{} // Shouldn't be called

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
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

	_, _, _, err = env.ExecEnv(context.Background(), chain, "test", taskengine.DataTypeString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "input variable")
}

func TestUnit_SimpleEnv_ExecEnv_InputVar_DefaultBehavior(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence:          []any{"first", "second"},
		MockTransitionValueSequence: []string{"first", "second"},
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), hooks.NewMockHookRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:             "task1",
				Handler:        taskengine.HandlePromptToString,
				PromptTemplate: `First: {{.input}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: "task2"},
					},
				},
			},
			{
				ID:      "task2",
				Handler: taskengine.HandlePromptToString,
				// No InputVar specified - should use previous output
				PromptTemplate: `Second: {{.task1}}`,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: "default", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	result, _, _, err := env.ExecEnv(context.Background(), chain, "input", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, "second", result)
}
