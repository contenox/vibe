package taskengine_test

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestUnit_EdgeTraversedAtLeast_BoundsCyclicWorkflow verifies that an
// edge_traversed_at_least branch placed ahead of a normal loop branch
// intercepts the loop after exactly N traversals of the named edge.
//
// Chain shape: chat <-> run_tools loop, with a budget branch out to
// summariser when chat->run_tools has fired the threshold many times.
func TestUnit_EdgeTraversedAtLeast_BoundsCyclicWorkflow(t *testing.T) {
	const threshold = 3

	mockExec := &taskengine.MockTaskExecutor{
		MockTransitionValueSequence: []string{
			"tool_call", "", // round 1: chat -> run_tools, run_tools -> chat
			"tool_call", "", // round 2
			"tool_call", "", // round 3 (chat->run_tools count reaches 3)
			"tool_call", // round 4: chat re-enters; budget branch should win regardless of this value
			"",          // summariser default-branches to end
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

	// chat: enters at rounds 1, 2, 3, 4 (4×). run_tools: 3×. summariser: 1×.
	// Total task invocations = 8.
	require.Equal(t, 8, mockExec.CallCount(), "expected 8 task invocations: 4×chat + 3×run_tools + 1×summariser")
	_ = threshold
}

// TestUnit_EdgeTraversedAtLeast_DoesNotFireBelowThreshold verifies that
// a budget branch with threshold N does NOT intercept when the count is N-1.
// The chain should terminate naturally via the default branch.
func TestUnit_EdgeTraversedAtLeast_DoesNotFireBelowThreshold(t *testing.T) {
	mockExec := &taskengine.MockTaskExecutor{
		MockTransitionValueSequence: []string{
			"tool_call", "", // round 1
			"tool_call", "", // round 2
			"done", // round 3: chat returns non-tool-call, default branch -> end
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
							When:     "10", // never reached
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
	// chat enters at rounds 1, 2, 3 (3×); run_tools at 1, 2 (2×); summariser never. Total 5.
	require.Equal(t, 5, mockExec.CallCount(), "expected 5 invocations: chain ends via default branch before budget reached")
}

// TestUnit_EdgeTraversedAtLeast_RejectedByValidator verifies that malformed
// edge fields are caught by validateChain before execution.
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
