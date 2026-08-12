package taskengine_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestUnit_EdgeTraversedAtLeast_BoundsCyclicWorkflow verifies an edge_traversed_at_least branch ahead of a normal loop branch intercepts after exactly N traversals of the named edge.
func TestUnit_EdgeTraversedAtLeast_BoundsCyclicWorkflow(t *testing.T) {
	const threshold = 3

	mockExec := &taskengine.MockTaskExecutor{
		MockTransitionValueSequence: []string{
			"tool_call", "",
			"tool_call", "",
			"tool_call", "",
			"tool_call",
			"",
		},
		MockOutput: "stub",
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "chat",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpEdgeTraversedAtLeast,
							Edge:     "chat->run_tools",
							When:     "3",
							Goto:     "summariser",
						},
						{Operator: taskengine.OpEquals, When: "tool_call", Goto: "run_tools"},
					},
				},
			},
			{
				ID:      "run_tools",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "chat"},
					},
				},
			},
			{
				ID:      "summariser",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "go", taskengine.DataTypeString)
	require.NoError(t, err)

	require.Equal(t, 8, mockExec.CallCount(), "expected 8 task invocations: 4×chat + 3×run_tools + 1×summariser")
	_ = threshold
}

// TestUnit_EdgeTraversedAtLeast_DoesNotFireBelowThreshold verifies a budget branch with threshold N does not intercept when the count is N-1.
func TestUnit_EdgeTraversedAtLeast_DoesNotFireBelowThreshold(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockTransitionValueSequence: []string{
			"tool_call", "",
			"tool_call", "",
			"done",
		},
		MockOutput: "stub",
	}

	tracker := libtracker.NoopTracker{}
	env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		Tasks: []taskengine.TaskDefinition{
			{
				ID:      "chat",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpEdgeTraversedAtLeast,
							Edge:     "chat->run_tools",
							When:     "10",
							Goto:     "summariser",
						},
						{Operator: taskengine.OpEquals, When: "tool_call", Goto: "run_tools"},
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
			{
				ID:      "run_tools",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: "chat"},
					},
				},
			},
			{
				ID:      "summariser",
				Handler: taskengine.HandleNoop,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "go", taskengine.DataTypeString)
	require.NoError(t, err)
	require.Equal(t, 5, mockExec.CallCount(), "expected 5 invocations: chain ends via default branch before budget reached")
}

// TestUnit_EdgeTraversedAtLeast_RejectedByValidator verifies malformed edge fields are caught by validateChain before execution.
func TestUnit_EdgeTraversedAtLeast_RejectedByValidator(t *testing.T) {
	cases := []struct {
		name string
		edge string
		when string
	}{
		{"missing edge", "", "5"},
		{"malformed edge", "chat:run_tools", "5"},
		{"unknown source task", "ghost->run_tools", "5"},
		{"unknown target task", "chat->ghost", "5"},
		{"non-integer threshold", "chat->run_tools", "many"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockExec := &taskengine.MockTaskExecutor{MockOutput: "stub", MockTransitionValue: ""}
			tracker := libtracker.NoopTracker{}
			env, err := taskengine.NewEnv(context.Background(), tracker, mockExec, taskengine.NewSimpleInspector(), tools.NewMockToolsRegistry())
			require.NoError(t, err)

			chain := &taskengine.TaskChainDefinition{
				Tasks: []taskengine.TaskDefinition{
					{
						ID:      "chat",
						Handler: taskengine.HandleNoop,
						Transition: taskengine.TaskTransition{
							Branches: []taskengine.TransitionBranch{
								{Operator: taskengine.OpEdgeTraversedAtLeast, Edge: tc.edge, When: tc.when, Goto: "run_tools"},
								{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
							},
						},
					},
					{
						ID:      "run_tools",
						Handler: taskengine.HandleNoop,
						Transition: taskengine.TaskTransition{
							Branches: []taskengine.TransitionBranch{
								{Operator: taskengine.OpDefault, Goto: taskengine.TermEnd},
							},
						},
					},
				},
			}
			_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "go", taskengine.DataTypeString)
			require.Error(t, err)
		})
	}
}
