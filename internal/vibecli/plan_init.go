package vibecli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/vibe/taskengine"
)

//go:embed chain-planner.json
var chainPlanner string

//go:embed chain-step-executor.json
var chainStepExecutor string

// ensurePlanChains writes the planner and step executor chains to the contenoxDir (always overwriting).
// Returns the absolute paths to the two chains.
func ensurePlanChains(contenoxDir string) (plannerPath, executorPath string, err error) {
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return "", "", fmt.Errorf("failed to create config dir: %w", err)
	}

	plannerPath = filepath.Join(contenoxDir, "chain-planner.json")
	if err := os.WriteFile(plannerPath, []byte(chainPlanner), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write %s: %w", plannerPath, err)
	}

	executorPath = filepath.Join(contenoxDir, "chain-step-executor.json")
	if err := os.WriteFile(executorPath, []byte(chainStepExecutor), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write %s: %w", executorPath, err)
	}

	return plannerPath, executorPath, nil
}

// validatePlannerChain checks that a chain meets the contract required by 'vibe plan new'.
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
				"  'vibe plan new' sends the goal as a user message and expects the chain to return\n"+
				"  a JSON object: {\"steps\": [{\"description\": \"...\"}]}\n"+
				"  The first task must be a chat_completion that generates this structure.",
			path, first.ID, first.Handler, taskengine.HandleChatCompletion,
		)
	}
	return nil
}

// validateExecutorChain checks that a chain meets the contract required by 'vibe plan next'.
// The executor chain must:
//   - Have at least one chat_completion task
//   - Have at least one execute_tool_calls task (to run local_shell/local_fs tools)
//   - Have a branch that routes "tool-call" transitions to the execute_tool_calls task
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
				"  'vibe plan next' sends each plan step as a prompt and expects the chain to use\n"+
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
	return nil
}
