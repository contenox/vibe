package taskengine_test

import (
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/require"
)

// lintChain builds a minimal chain around the given tasks.
func lintChain(tasks ...taskengine.TaskDefinition) *taskengine.TaskChainDefinition {
	return &taskengine.TaskChainDefinition{ID: "lint-test", Tasks: tasks}
}

func branchTo(op taskengine.OperatorTerm, when, goto_ string) taskengine.TransitionBranch {
	return taskengine.TransitionBranch{Operator: op, When: when, Goto: goto_}
}

// TestUnit_LintChain_GoodChainPasses exercises the shape every shipped agent
// chain uses: a chat loop with a tool-execution task fed via input_var, a
// recovery loop reached via on_failure, and a summarise task reading
// previous_output. Nothing here may error.
func TestUnit_LintChain_GoodChainPasses(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "chat", Handler: taskengine.HandleChatCompletion,
			Transition: taskengine.TaskTransition{
				OnFailure: "summarise",
				Branches: []taskengine.TransitionBranch{
					branchTo(taskengine.OpEquals, taskengine.TransitionToolCall, "run_tools"),
					branchTo(taskengine.OpDefault, "", taskengine.TermEnd),
				},
			},
		},
		taskengine.TaskDefinition{
			ID: "run_tools", Handler: taskengine.HandleExecuteToolCalls, InputVar: "chat",
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "chat")},
			},
		},
		taskengine.TaskDefinition{
			ID: "summarise", Handler: taskengine.HandleChatCompletion, InputVar: "previous_output",
			Print: "chat said: {{.chat}} (error: {{.chat_error}})",
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", taskengine.TermEnd)},
			},
		},
	)
	require.NoError(t, taskengine.LintChain(chain))
	// And with a concrete entry type the runtime actually uses.
	require.NoError(t, taskengine.LintChain(chain, taskengine.DataTypeChatHistory))
	require.NoError(t, taskengine.LintChain(chain, taskengine.DataTypeString))
}

// TestUnit_LintChain_ImpossibleEdgeIsLoadError: a task that can only produce
// a string feeding a handler that only accepts chat_history is provably
// broken — the error must name both endpoints and both type sets.
func TestUnit_LintChain_ImpossibleEdgeIsLoadError(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "extract", Handler: taskengine.HandleNoop,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpEquals, taskengine.TransitionNoop, "summarize")},
			},
		},
		taskengine.TaskDefinition{
			ID: "summarize", Handler: taskengine.HandleExecuteToolCalls,
		},
	)
	err := taskengine.LintChain(chain, taskengine.DataTypeString)
	require.Error(t, err)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(),
		"task[summarize] handler execute_tool_calls cannot accept input from task[extract] (produces string; accepts chat_history)")
}

// TestUnit_LintChain_AnyFlowsStayRuntimeChecked: the eino tri-state's middle
// value. A tools task's output type is unknowable at load, so an edge carrying
// it must pass the linter and stay the runtime backstop's problem.
func TestUnit_LintChain_AnyFlowsStayRuntimeChecked(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "fetch", Handler: taskengine.HandleTools,
			Tools: &taskengine.ToolsCall{Name: "webtools", ToolName: "web_get"},
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "digest")},
			},
		},
		// execute_tool_calls has the narrowest accept set; only Any keeps this legal.
		taskengine.TaskDefinition{ID: "digest", Handler: taskengine.HandleExecuteToolCalls},
	)
	require.NoError(t, taskengine.LintChain(chain, taskengine.DataTypeString))

	// An unknown chain entry type (default) likewise flows as Any.
	chain2 := lintChain(taskengine.TaskDefinition{ID: "digest", Handler: taskengine.HandleExecuteToolCalls})
	require.NoError(t, taskengine.LintChain(chain2))
}

// TestUnit_LintChain_OutputTemplateNarrowsToolsToString: with an
// output_template the tools task's output is a rendered string, which makes a
// downstream execute_tool_calls provably impossible.
func TestUnit_LintChain_OutputTemplateNarrowsToolsToString(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "fetch", Handler: taskengine.HandleTools,
			Tools:          &taskengine.ToolsCall{Name: "webtools", ToolName: "web_get"},
			OutputTemplate: "status: {{.status}}",
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "digest")},
			},
		},
		taskengine.TaskDefinition{ID: "digest", Handler: taskengine.HandleExecuteToolCalls},
	)
	err := taskengine.LintChain(chain, taskengine.DataTypeString)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(),
		"task[digest] handler execute_tool_calls cannot accept input from task[fetch] (produces string; accepts chat_history)")
}

// TestUnit_LintChain_PromptTemplateIntoExecuteToolCalls: a prompt_template
// replaces the input with a rendered string on every path, so declaring one on
// an execute_tool_calls task can never work.
func TestUnit_LintChain_PromptTemplateIntoExecuteToolCalls(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "run_tools", Handler: taskengine.HandleExecuteToolCalls,
			PromptTemplate: "{{.input}}",
		},
	)
	err := taskengine.LintChain(chain, taskengine.DataTypeChatHistory)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), "cannot take a prompt_template")
}

// TestUnit_LintChain_InputVarNeverProduced: naming a variable no predecessor
// writes fails at runtime with "input variable not found"; the linter teaches
// it at load and lists what IS available.
func TestUnit_LintChain_InputVarNeverProduced(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "chat", Handler: taskengine.HandleChatCompletion,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "next")},
			},
		},
		taskengine.TaskDefinition{
			ID: "next", Handler: taskengine.HandleChatCompletion, InputVar: "chatt",
		},
	)
	err := taskengine.LintChain(chain)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), `input_var "chatt" is never produced by any task that can run before it`)
	require.Contains(t, err.Error(), "variables available here: chat, input, previous_output")
}

// TestUnit_LintChain_InputVarTypeMismatch: the variable exists but can only
// ever hold a type the handler rejects.
func TestUnit_LintChain_InputVarTypeMismatch(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "start", Handler: taskengine.HandleNoop,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "chat")},
			},
		},
		taskengine.TaskDefinition{
			ID: "chat", Handler: taskengine.HandleChatCompletion, InputVar: "input",
		},
	)
	err := taskengine.LintChain(chain, taskengine.DataTypeInt)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(),
		`task[chat] handler chat_completion cannot accept input from input_var "input" (produces int; accepts string, chat_history)`)
}

// TestUnit_LintChain_DeadBranchOnClosedVocabulary: an equals branch against a
// token the handler can never emit (here the retired hyphenated spelling of
// tool_call) is a dead branch and a load error.
func TestUnit_LintChain_DeadBranchOnClosedVocabulary(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "chat", Handler: taskengine.HandleChatCompletion,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{
					branchTo(taskengine.OpEquals, "tool-call", "run_tools"),
					branchTo(taskengine.OpDefault, "", taskengine.TermEnd),
				},
			},
		},
		taskengine.TaskDefinition{ID: "run_tools", Handler: taskengine.HandleExecuteToolCalls, InputVar: "chat"},
	)
	err := taskengine.LintChain(chain)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), `branch (equals "tool-call") can never fire`)
	require.Contains(t, err.Error(), "tool_call, executed")
}

// TestUnit_LintChain_RouteNeedsEqualsBranches mirrors the runtime refusal
// ("route task has no equals branches to route between") at load time.
func TestUnit_LintChain_RouteNeedsEqualsBranches(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "classify", Handler: taskengine.HandleRoute,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", taskengine.TermEnd)},
			},
		},
	)
	err := taskengine.LintChain(chain)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), "a route task needs at least one equals branch")
}

// TestUnit_LintChain_MacroChecks: edge_count macros must name real
// task-to-task edges, and {{var:}} must name a variable.
func TestUnit_LintChain_MacroChecks(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "chat", Handler: taskengine.HandleChatCompletion,
			SystemInstruction: "Used {{edge_count:chat->run_toolz}} rounds; {{edge_count:chat->end}}; model {{var:}}",
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{
					branchTo(taskengine.OpEquals, taskengine.TransitionToolCall, "run_tools"),
					branchTo(taskengine.OpDefault, "", taskengine.TermEnd),
				},
			},
		},
		taskengine.TaskDefinition{ID: "run_tools", Handler: taskengine.HandleExecuteToolCalls, InputVar: "chat"},
	)
	err := taskengine.LintChain(chain)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), `{{edge_count:chat->run_toolz}} references unknown task "run_toolz"`)
	require.Contains(t, err.Error(), "counts an edge into 'end', which is never incremented")
	require.Contains(t, err.Error(), "{{var:}} names no variable")
}

// TestUnit_LintChain_TemplateRefToUnknownVar: a {{.typo}} renders as the
// literal "<no value>" at runtime; the linter names the typo.
func TestUnit_LintChain_TemplateRefToUnknownVar(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "report", Handler: taskengine.HandleChatCompletion,
			PromptTemplate: "summarise {{.summry}} briefly",
		},
	)
	err := taskengine.LintChain(chain)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), "references {{.summry}}, but no task or engine variable with that name exists")
}

// TestUnit_LintChain_StructuralDefectsWrapErrChainLint: the linter delegates
// structure (duplicate ids, unknown goto targets, unknown handlers) to the
// runtime's validateChain and marks the result as a lint failure.
func TestUnit_LintChain_StructuralDefectsWrapErrChainLint(t *testing.T) {
	dup := lintChain(
		taskengine.TaskDefinition{ID: "a", Handler: taskengine.HandleNoop},
		taskengine.TaskDefinition{ID: "a", Handler: taskengine.HandleNoop},
	)
	err := taskengine.LintChain(dup)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), `duplicate task ID "a"`)

	badGoto := lintChain(
		taskengine.TaskDefinition{
			ID: "a", Handler: taskengine.HandleNoop,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "gone")},
			},
		},
	)
	err = taskengine.LintChain(badGoto)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), `goto references unknown task "gone"`)

	badHandler := lintChain(taskengine.TaskDefinition{ID: "a", Handler: "prompt"})
	err = taskengine.LintChain(badHandler)
	require.ErrorIs(t, err, taskengine.ErrChainLint)
	require.Contains(t, err.Error(), `unknown handler "prompt"`)
}

// TestUnit_LintChain_RaiseErrorSuccessorsUnreachable: raise_error never
// succeeds, so a goto branch from it must not make its target's dataflow
// reachable through that edge — but the on_failure edge does flow.
func TestUnit_LintChain_RaiseErrorFlowsOnlyThroughOnFailure(t *testing.T) {
	chain := lintChain(
		taskengine.TaskDefinition{
			ID: "fail", Handler: taskengine.HandleRaiseError,
			Transition: taskengine.TaskTransition{
				OnFailure: "explain",
				Branches:  []taskengine.TransitionBranch{branchTo(taskengine.OpDefault, "", "explain")},
			},
		},
		taskengine.TaskDefinition{ID: "explain", Handler: taskengine.HandleChatCompletion, InputVar: "last_error"},
	)
	// last_error is a string, which chat_completion accepts: legal.
	require.NoError(t, taskengine.LintChain(chain, taskengine.DataTypeString))
}
