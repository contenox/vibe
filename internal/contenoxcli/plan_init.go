package contenoxcli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/taskengine"
)

//go:embed chain-planner.json
var chainPlanner string

//go:embed chain-step-executor.json
var chainStepExecutor string

// writeEmbeddedPlanChains writes chain-planner.json and chain-step-executor.json from the same
// embedded bytes used by the plan subcommands. If overwrite is false, an existing file is left
// untouched (returns wrotePlanner/wroteExecutor false for that file).
func writeEmbeddedPlanChains(contenoxDir string, overwrite bool) (plannerPath, executorPath string, wrotePlanner, wroteExecutor bool, err error) {
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return "", "", false, false, fmt.Errorf("failed to create config dir: %w", err)
	}

	plannerPath = filepath.Join(contenoxDir, "chain-planner.json")
	executorPath = filepath.Join(contenoxDir, "chain-step-executor.json")

	writeOne := func(path, content string) (bool, error) {
		if !overwrite {
			if _, statErr := os.Stat(path); statErr == nil {
				return false, nil
			}
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return false, fmt.Errorf("failed to write %s: %w", path, err)
		}
		return true, nil
	}

	wrotePlanner, err = writeOne(plannerPath, chainPlanner)
	if err != nil {
		return "", "", false, false, err
	}
	wroteExecutor, err = writeOne(executorPath, chainStepExecutor)
	if err != nil {
		return "", "", wrotePlanner, false, err
	}
	return plannerPath, executorPath, wrotePlanner, wroteExecutor, nil
}

// ensurePlanChains writes the planner and step executor chains to the contenoxDir (always overwriting).
// Returns the absolute paths to the two chains.
func ensurePlanChains(contenoxDir string) (plannerPath, executorPath string, err error) {
	p, e, _, _, err := writeEmbeddedPlanChains(contenoxDir, true)
	return p, e, err
}

// validatePlannerChain checks that a chain meets the contract required by 'contenox plan new'.
// The planner chain must:
//   - Have at least one task
//   - Have a chat_completion task as its first task (to generate the JSON step list)
func validatePlannerChain(chain *taskengine.TaskChainDefinition, path string) error {
	if len(chain.Tasks) == 0 {
		return fmt.Errorf(
			"planner chain %q has no tasks\n"+
				"  The planner chain must have at least one task with handler \"chat_completion\".\n"+
				"  The model output must be a JSON object: {\"steps\": [{\"description\": \"...\"}]}",
			path,
		)
	}
	first := chain.Tasks[0]
	if first.Handler != taskengine.HandleChatCompletion {
		return fmt.Errorf(
			"planner chain %q: first task %q has handler %q, expected %q\n"+
				"  'contenox plan new' sends the goal as a user message and expects the chain to return\n"+
				"  a JSON object: {\"steps\": [{\"description\": \"...\"}]}\n"+
				"  The first task must be a chat_completion that generates this structure.",
			path, first.ID, first.Handler, taskengine.HandleChatCompletion,
		)
	}
	return nil
}

// validateExecutorChain checks that a chain meets the contract required by 'contenox plan next'.
// The executor chain must:
//   - Have at least one chat_completion task
//   - Have at least one execute_tool_calls task (to run local_shell/local_fs tools)
//   - Have a branch that routes "tool-call" transitions to the execute_tool_calls task
//   - Close the agentic loop: each execute_tool_calls task must branch back to its input_var
//     chat task (same pattern as chain-contenox: chat ↔ tools until no tool calls).
func validateExecutorChain(chain *taskengine.TaskChainDefinition, path string) error {
	if len(chain.Tasks) == 0 {
		return fmt.Errorf(
			"executor chain %q has no tasks\n"+
				"  The executor chain must contain a \"chat_completion\" task and an \"execute_tool_calls\" task.",
			path,
		)
	}

	hasChatCompletion := false
	hasToolExec := false
	hasToolCallBranch := false

	for _, task := range chain.Tasks {
		if task.Handler == taskengine.HandleChatCompletion {
			hasChatCompletion = true
			for _, branch := range task.Transition.Branches {
				if branch.When == "tool-call" {
					hasToolCallBranch = true
				}
			}
		}
		if task.Handler == taskengine.HandleExecuteToolCalls {
			hasToolExec = true
		}
	}

	if !hasChatCompletion {
		return fmt.Errorf(
			"executor chain %q: no task with handler %q found\n"+
				"  'contenox plan next' sends each plan step as a prompt and expects the chain to use\n"+
				"  tools (local_shell, local_fs) and output ===STEP_DONE=== when complete.",
			path, taskengine.HandleChatCompletion,
		)
	}
	if !hasToolExec {
		return fmt.Errorf(
			"executor chain %q: no task with handler %q found\n"+
				"  The executor chain needs an \"execute_tool_calls\" task to run tool calls made by the model.\n"+
				"  Without it, local_shell and local_fs tools will not execute.",
			path, taskengine.HandleExecuteToolCalls,
		)
	}
	if !hasToolCallBranch {
		return fmt.Errorf(
			"executor chain %q: chat_completion task has no branch for operator \"equals\" when \"tool-call\"\n"+
				"  Without this branch, tool calls from the model are never dispatched to \"execute_tool_calls\".\n"+
				"  Add a branch: {\"operator\": \"equals\", \"when\": \"tool-call\", \"goto\": \"<your_tool_exec_task_id>\"}",
			path,
		)
	}
	if err := validateAgenticLoopClosure(chain, path); err != nil {
		return err
	}
	return nil
}

// validateAgenticLoopClosure ensures each execute_tool_calls task branches back to its chat task
// (input_var), matching the engine's standard agentic loop used in chain-contenox.
func validateAgenticLoopClosure(chain *taskengine.TaskChainDefinition, path string) error {
	for _, task := range chain.Tasks {
		if task.Handler != taskengine.HandleExecuteToolCalls {
			continue
		}
		chatID := strings.TrimSpace(task.InputVar)
		if chatID == "" {
			return fmt.Errorf(
				"executor chain %q: task %q has handler %q but empty input_var\n"+
					"  Set input_var to your chat_completion task id so tool results append to the same history.",
				path, task.ID, task.Handler,
			)
		}
		loops := false
		for _, b := range task.Transition.Branches {
			if strings.TrimSpace(b.Goto) == chatID {
				loops = true
				break
			}
		}
		if !loops {
			return fmt.Errorf(
				"executor chain %q: agentic loop not closed — task %q (execute_tool_calls) needs a transition branch with \"goto\": %q\n"+
					"  After tools run, the chain must return to the chat task (same id as input_var), like chain-contenox run_tools → contenox_chat.",
				path, task.ID, chatID,
			)
		}
	}
	return nil
}
