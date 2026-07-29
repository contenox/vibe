package taskengine_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/kernel/tools"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// stepRecordingExec records the SystemInstruction it sees on each TaskExec
// invocation for the named task. It delegates the actual transition value to a
// caller-supplied sequence so tests can drive multi-step loops.
type stepRecordingExec struct {
	watchTaskID string
	seenSystem  []string
	transitions []string
}

func (r *stepRecordingExec) TaskExec(
	_ context.Context,
	_ time.Time,
	_ int,
	_ *taskengine.ChainContext,
	currentTask *taskengine.TaskDefinition,
	input any,
	dataType taskengine.DataType,
) (any, taskengine.DataType, string, error) {
	if currentTask.ID == r.watchTaskID {
		r.seenSystem = append(r.seenSystem, currentTask.SystemInstruction)
	}

	var trans string
	if len(r.transitions) > 0 {
		trans = r.transitions[0]
		r.transitions = r.transitions[1:]
	}
	return input, dataType, trans, nil
}

// TestUnit_StepMacro_EdgeCountGrowsAcrossLoopIterations verifies that
// {{edge_count:from->to}} is re-evaluated on each task step against the live
// edge counters. This is the foundation for putting a dynamic budget readout
// in a single chat task's system_instruction — eliminating the need for a
// separate recovery_chat task with a frozen "10 of 20" string.
func TestUnit_StepMacro_EdgeCountGrowsAcrossLoopIterations(t *testing.T) {
	rec := &stepRecordingExec{
		watchTaskID: "chat",
		transitions: []string{
			"tool_call", "", // round 1: chat -> run_tools, run_tools -> chat
			"tool_call", "", // round 2
			"done", // round 3: chat default-branches to end
		},
	}

	env, err := taskengine.NewEnv(
		context.Background(),
		libtracker.NoopTracker{},
		rec,
		taskengine.NewSimpleInspector(),
		tools.NewMockToolsRegistry(),
	)
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "edge-count-loop",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:                "chat",
				Handler:           taskengine.HandleNoop,
				SystemInstruction: "BUDGET: {{edge_count:chat->run_tools}}/20",
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
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
		},
	}

	_, _, _, err = env.ExecEnv(libtracker.WithNewRequestID(context.Background()), chain, "go", taskengine.DataTypeString)
	require.NoError(t, err)

	require.Equal(t, []string{
		"BUDGET: 0/20",
		"BUDGET: 1/20",
		"BUDGET: 2/20",
	}, rec.seenSystem, "the macro must reflect the live edge count at each entry to chat — without this, a single-agent dynamic budget prompt is impossible and we are forced back into the recovery_chat split")
}

// TestUnit_StepMacro_NoMacroIsZeroCost is a sanity check that the fast-path
// short-circuit kicks in for task strings that don't reference the macro, so
// existing chains pay no per-step regex cost.
func TestUnit_StepMacro_NoMacroIsZeroCost(t *testing.T) {
	rec := &stepRecordingExec{watchTaskID: "chat"}
	env, err := taskengine.NewEnv(
		context.Background(),
		libtracker.NoopTracker{},
		rec,
		taskengine.NewSimpleInspector(),
		tools.NewMockToolsRegistry(),
	)
	require.NoError(t, err)

	const original = "Just a plain system instruction — no macro here."
	chain := &taskengine.TaskChainDefinition{
		ID: "no-macro",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:                "chat",
				Handler:           taskengine.HandleNoop,
				SystemInstruction: original,
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

	require.Equal(t, []string{original}, rec.seenSystem, "the macro pass must not mutate plain strings")
}

// TestUnit_StepMacro_UnknownEdgeIsZero verifies the macro resolves to 0 when
// the referenced edge has never been traversed (e.g. a chain author typo, or a
// branch that was about to be taken but isn't yet). Better to read "0" than
// surface a hard error mid-prompt, which would derail the turn.
func TestUnit_StepMacro_UnknownEdgeIsZero(t *testing.T) {
	rec := &stepRecordingExec{watchTaskID: "chat"}
	env, err := taskengine.NewEnv(
		context.Background(),
		libtracker.NoopTracker{},
		rec,
		taskengine.NewSimpleInspector(),
		tools.NewMockToolsRegistry(),
	)
	require.NoError(t, err)

	chain := &taskengine.TaskChainDefinition{
		ID: "unknown-edge",
		Tasks: []taskengine.TaskDefinition{
			{
				ID:                "chat",
				Handler:           taskengine.HandleNoop,
				SystemInstruction: "rounds={{edge_count:nope->never}}",
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

	require.Equal(t, []string{"rounds=0"}, rec.seenSystem)
}
