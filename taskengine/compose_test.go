package taskengine_test

import (
	"context"
	"testing"

	"github.com/contenox/runtime/internal/hooks"
	"github.com/contenox/runtime/libtracker"
	"github.com/contenox/runtime/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_BranchComposeOverride(t *testing.T) {
	// Setup environment with proper mock sequence
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence: []any{
			map[string]any{"a": 1, "b": 2},
			map[string]any{"b": 3, "c": 4},
		},
	}
	env := setupTestEnv(mockExec)

	// Define chain with branch-specific compose
	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandlePromptToString,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "task2"},
					},
				},
			},
			{
				ID:      "task2",
				Handler: taskengine.HandlePromptToString,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpDefault,
							Goto:     taskengine.TermEnd,
							Compose: &taskengine.BranchCompose{
								WithVar:  "task1",
								Strategy: "override",
							},
						},
					},
				},
			},
		},
	}

	// Execute chain
	output, _, _, err := env.ExecEnv(context.Background(), chain, map[string]any{"a": 1, "b": 2}, taskengine.DataTypeJSON)
	require.NoError(t, err)

	// Verify composition
	expected := map[string]any{"a": 1, "b": 3, "c": 4}
	assert.EqualValues(t, expected, output)
}

func TestUnit_BranchComposeAppendStringToChatHistory(t *testing.T) {
	// Setup environment
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence: []any{
			"New system message", // Task1 output (string)
			taskengine.ChatHistory{ // Task2 output (ChatHistory)
				Messages: []taskengine.Message{
					{Role: "user", Content: "Hello"},
				},
			},
		},
	}
	env := setupTestEnv(mockExec)

	// Define chain
	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandlePromptToString,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "task2"},
					},
				},
			},
			{
				ID:      "task2",
				Handler: taskengine.HandlePromptToString,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpDefault,
							Goto:     taskengine.TermEnd,
							Compose: &taskengine.BranchCompose{
								WithVar:  "task1",
								Strategy: "append_string_to_chat_history",
							},
						},
					},
				},
			},
		},
	}

	// Execute chain
	output, _, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeAny)
	require.NoError(t, err)

	// Verify composition: append_string_to_chat_history appends the string as assistant to the end of the history
	ch, ok := output.(taskengine.ChatHistory)
	require.True(t, ok, "output should be ChatHistory")
	require.Len(t, ch.Messages, 2)
	assert.Equal(t, "user", ch.Messages[0].Role)
	assert.Equal(t, "Hello", ch.Messages[0].Content)
	assert.Equal(t, "assistant", ch.Messages[1].Role)
	assert.Equal(t, "New system message", ch.Messages[1].Content)
}

func TestUnit_BranchComposeMergeChatHistories(t *testing.T) {
	// Setup environment
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence: []any{
			taskengine.ChatHistory{ // Task1 output
				Messages: []taskengine.Message{
					{Role: "user", Content: "Hello"},
				},
				InputTokens: 10,
			},
			taskengine.ChatHistory{ // Task2 output
				Messages: []taskengine.Message{
					{Role: "assistant", Content: "Hi there!"},
				},
				InputTokens: 20,
				Model:       "gpt-4",
			},
		},
		MockTaskTypeSequence: []taskengine.DataType{
			taskengine.DataTypeChatHistory,
			taskengine.DataTypeChatHistory,
		},
	}
	env := setupTestEnv(mockExec)

	// Define chain
	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "task2"},
					},
				},
			},
			{
				ID:      "task2",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpDefault,
							Goto:     taskengine.TermEnd,
							Compose: &taskengine.BranchCompose{
								WithVar:  "task1",
								Strategy: "merge_chat_histories",
							},
						},
					},
				},
			},
		},
	}

	// Execute chain
	output, dt, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeChatHistory)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)
	// Verify composition
	ch, ok := output.(taskengine.ChatHistory)
	require.True(t, ok, "output should be ChatHistory")
	require.Len(t, ch.Messages, 2)
	assert.Equal(t, "user", ch.Messages[0].Role)
	assert.Equal(t, "Hello", ch.Messages[0].Content)
	assert.Equal(t, "assistant", ch.Messages[1].Role)
	assert.Equal(t, "Hi there!", ch.Messages[1].Content)
	assert.Equal(t, 30, ch.InputTokens)
	assert.Empty(t, ch.Model) // Models differ so should be empty
}

func TestUnit_BranchComposeAutoStrategy(t *testing.T) {
	t.Run("NonChatHistoryOverride", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{
			MockOutputSequence: []any{
				map[string]any{"a": 1},
				map[string]any{"b": 2},
			},
		}
		env := setupTestEnv(mockExec)

		// Define chain with automatic strategy
		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "task1",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: "task2"},
						},
					},
				},
				{
					ID:      "task2",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpDefault,
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar: "task1", // Strategy omitted for auto
								},
							},
						},
					},
				},
			},
		}

		// Execute chain
		output, _, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeAny)
		require.NoError(t, err)

		// Verify auto-selected override strategy
		result, ok := output.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, 1, result["a"])
		assert.Equal(t, 2, result["b"])
	})
}

func TestUnit_BranchComposeMergeChatHistories_MessageOrder(t *testing.T) {
	// Setup environment
	mockExec := &taskengine.MockTaskExecutor{
		MockOutputSequence: []any{
			taskengine.ChatHistory{ // Task1 output (user message)
				Messages: []taskengine.Message{
					{Role: "user", Content: "User message"},
				},
			},
			taskengine.ChatHistory{ // Task2 output (system message)
				Messages: []taskengine.Message{
					{Role: "system", Content: "System message"},
				},
			},
		},
		MockTaskTypeSequence: []taskengine.DataType{
			taskengine.DataTypeChatHistory,
			taskengine.DataTypeChatHistory,
		},
	}
	env := setupTestEnv(mockExec)

	// Define chain
	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "task1",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "task2"},
					},
				},
			},
			{
				ID:      "task2",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpDefault,
							Goto:     taskengine.TermEnd,
							Compose: &taskengine.BranchCompose{
								WithVar:  "task1", // Compose with task1 output
								Strategy: "merge_chat_histories",
							},
						},
					},
				},
			},
		},
	}

	// Execute chain
	output, dt, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeChatHistory)
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeChatHistory, dt)

	// Verify composition and message order
	ch, ok := output.(taskengine.ChatHistory)
	require.True(t, ok, "output should be ChatHistory")
	require.Len(t, ch.Messages, 2)

	// The right messages (task1 output) should come first
	assert.Equal(t, "user", ch.Messages[0].Role)
	assert.Equal(t, "User message", ch.Messages[0].Content)

	// The left messages (task2 output) should come after
	assert.Equal(t, "system", ch.Messages[1].Role)
	assert.Equal(t, "System message", ch.Messages[1].Content)
}

func TestUnit_BranchComposeErrors(t *testing.T) {
	t.Run("UnsupportedStrategy", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{MockOutput: "test"}
		env := setupTestEnv(mockExec)

		// Define chain with invalid strategy
		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "task1",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: "task2"},
						},
					},
				},
				{
					ID:      "task2",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpDefault,
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar:  "task1",
									Strategy: "invalid_strategy",
								},
							},
						},
					},
				},
			},
		}

		// Execute chain
		_, _, _, err := env.ExecEnv(context.Background(), chain, "input", taskengine.DataTypeString)

		// Verify error
		assert.ErrorContains(t, err, "unsupported compose strategy")
	})

	t.Run("MissingRightVar", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{MockOutput: "test"}
		env := setupTestEnv(mockExec)

		// Define chain with missing right variable
		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "task1",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpDefault,
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar: "nonexistent",
								},
							},
						},
					},
				},
			},
		}

		// Execute chain
		_, _, _, err := env.ExecEnv(context.Background(), chain, "input", taskengine.DataTypeString)

		// Verify error
		assert.ErrorContains(t, err, "compose right_var \"nonexistent\" not found")
	})

	t.Run("InvalidAppendStringTypes", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{
			MockOutputSequence: []any{
				[]string{}, // Invalid type
				taskengine.ChatHistory{},
			},
		}
		env := setupTestEnv(mockExec)

		// Define chain
		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "task1",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: "task2"},
						},
					},
				},
				{
					ID:      "task2",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpDefault,
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar:  "task1",
									Strategy: "append_string_to_chat_history",
								},
							},
						},
					},
				},
			},
		}

		// Execute chain
		_, _, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeAny)

		// Verify error
		assert.ErrorContains(t, err, "invalid types for append_string_to_chat_history")
	})

	t.Run("InvalidMergeChatHistoryTypes", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{
			MockOutputSequence: []any{
				"not a chat history",
				taskengine.ChatHistory{},
			},
		}
		env := setupTestEnv(mockExec)

		// Define chain
		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "task1",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: "task2"},
						},
					},
				},
				{
					ID:      "task2",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpDefault,
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar:  "task1",
									Strategy: "merge_chat_histories",
								},
							},
						},
					},
				},
			},
		}

		// Execute chain
		_, _, _, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeAny)
		assert.ErrorContains(t, err, "both values must be ChatHistory for merge")
	})
}

func TestUnit_BranchComposeConditionalPaths(t *testing.T) {
	t.Run("DifferentComposePerBranch", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{
			MockOutputSequence: []any{
				map[string]any{"status": "success", "data": "result1"},
				"approve", // Condition result
				map[string]any{"status": "success", "data": "result2"},
			},
		}
		env := setupTestEnv(mockExec)

		// Define chain with conditional branches and different compose operations
		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "process_data",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: "check_condition"},
						},
					},
				},
				{
					ID:      "check_condition",
					Handler: taskengine.HandlePromptToCondition,
					ValidConditions: map[string]bool{
						"approve": true,
						"reject":  true,
					},
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpEquals,
								When:     "approve",
								Goto:     "handle_approve",
								Compose: &taskengine.BranchCompose{
									WithVar:  "process_data",
									Strategy: "override",
								},
							},
							{
								Operator: taskengine.OpEquals,
								When:     "reject",
								Goto:     "handle_reject",
								Compose: &taskengine.BranchCompose{
									WithVar:  "input",
									Strategy: "override",
								},
							},
						},
					},
				},
				{
					ID:      "handle_approve",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
						},
					},
				},
				{
					ID:      "handle_reject",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
						},
					},
				},
			},
		}

		// Execute chain with initial input
		initialInput := map[string]any{"original": "data"}
		output, _, _, err := env.ExecEnv(context.Background(), chain, initialInput, taskengine.DataTypeJSON)
		require.NoError(t, err)

		// Verify that the approve branch was taken and composed with process_data
		result, ok := output.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "success", result["status"])
		assert.Equal(t, "result2", result["data"])
	})

	t.Run("NoComposeOnSomeBranches", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{
			MockOutputSequence: []any{
				"continue", // Condition result
			},
		}
		env := setupTestEnv(mockExec)

		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "router",
					Handler: taskengine.HandlePromptToCondition,
					ValidConditions: map[string]bool{
						"continue": true,
						"stop":     true,
					},
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpEquals,
								When:     "continue",
								Goto:     "next_task",
								// No compose - should pass through original output
							},
							{
								Operator: taskengine.OpEquals,
								When:     "stop",
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar:  "input",
									Strategy: "override",
								},
							},
						},
					},
				},
				{
					ID:      "next_task",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
						},
					},
				},
			},
		}

		initialInput := "original input"
		output, _, _, err := env.ExecEnv(context.Background(), chain, initialInput, taskengine.DataTypeString)
		require.NoError(t, err)

		// Should pass through the original input since no compose was applied
		assert.Equal(t, "continue", output)
	})
}

func TestUnit_BranchComposeVariableTracking(t *testing.T) {
	t.Run("BranchSpecificVariables", func(t *testing.T) {
		// Setup environment
		mockExec := &taskengine.MockTaskExecutor{
			MockOutputSequence: []any{
				map[string]any{"task1": "data"},
				map[string]any{"task2": "data"},
			},
		}
		env := setupTestEnv(mockExec)

		chain := &taskengine.TaskChainDefinition{
			Tasks: []taskengine.TaskDefinition{
				{
					ID:      "task1",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{Operator: taskengine.OpDefault, Goto: "task2"},
						},
					},
				},
				{
					ID:      "task2",
					Handler: taskengine.HandlePromptToString,
					Transition: taskengine.TaskTransition{
						Branches: []taskengine.TransitionBranch{
							{
								Operator: taskengine.OpDefault,
								Goto:     taskengine.TermEnd,
								Compose: &taskengine.BranchCompose{
									WithVar:  "task1",
									Strategy: "override",
								},
							},
						},
					},
				},
			},
		}

		_, _, capturedState, err := env.ExecEnv(context.Background(), chain, nil, taskengine.DataTypeAny)
		require.NoError(t, err)

		// Verify that the execution completed successfully
		assert.NotEmpty(t, capturedState)
		// The last state should be for task2
		lastState := capturedState[len(capturedState)-1]
		assert.Equal(t, "task2", lastState.TaskID)
	})
}

// Helper to create test environment
func setupTestEnv(exec taskengine.TaskExecutor) taskengine.EnvExecutor {
	// Create no-op dependencies
	tracker := &libtracker.NoopTracker{}
	inspector := taskengine.NewSimpleInspector()

	env, _ := taskengine.NewEnv(
		context.Background(),
		tracker,
		exec,
		inspector,
		hooks.NewMockHookRegistry(),
	)
	return env
}
