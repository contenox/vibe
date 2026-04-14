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

//go:embed chain-step-executor-gated.json
var chainStepExecutorGated string

//go:embed chain-plan-explorer.json
var chainPlanExplorer string

//go:embed chain-step-summarizer.json
var chainStepSummarizer string

// writeEmbeddedPlanChains writes chain-planner.json, chain-step-executor.json,
// chain-step-executor-gated.json, chain-plan-explorer.json, and
// chain-step-summarizer.json from the same embedded bytes used by the plan
// subcommands. If overwrite is false, an existing file is left untouched
// (returns false for that file).
func writeEmbeddedPlanChains(contenoxDir string, overwrite bool) (plannerPath, executorPath, summarizerPath string, wrotePlanner, wroteExecutor, wroteSummarizer bool, err error) {
	if err := os.MkdirAll(contenoxDir, 0750); err != nil {
		return "", "", "", false, false, false, fmt.Errorf("failed to create config dir: %w", err)
	}

	plannerPath = filepath.Join(contenoxDir, "chain-planner.json")
	executorPath = filepath.Join(contenoxDir, "chain-step-executor.json")
	summarizerPath = filepath.Join(contenoxDir, "chain-step-summarizer.json")
	executorGatedPath := filepath.Join(contenoxDir, "chain-step-executor-gated.json")
	explorerPath := filepath.Join(contenoxDir, "chain-plan-explorer.json")

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
		return "", "", "", false, false, false, err
	}
	wroteExecutor, err = writeOne(executorPath, chainStepExecutor)
	if err != nil {
		return "", "", "", wrotePlanner, false, false, err
	}
	if _, werr := writeOne(executorGatedPath, chainStepExecutorGated); werr != nil {
		return "", "", "", wrotePlanner, wroteExecutor, false, werr
	}
	if _, werr := writeOne(explorerPath, chainPlanExplorer); werr != nil {
		return "", "", "", wrotePlanner, wroteExecutor, false, werr
	}
	wroteSummarizer, err = writeOne(summarizerPath, chainStepSummarizer)
	if err != nil {
		return "", "", "", wrotePlanner, wroteExecutor, false, err
	}
	return plannerPath, executorPath, summarizerPath, wrotePlanner, wroteExecutor, wroteSummarizer, nil
}

// ensurePlanChains writes the planner, step executor, gated executor, plan
// explorer, and step summarizer chains to the contenoxDir (always overwriting).
// Returns the absolute paths to the three primary chains (planner, executor,
// summarizer). The gated executor and plan explorer files are written alongside
// for use with plan next --gate and plan explore respectively.
func ensurePlanChains(contenoxDir string) (plannerPath, executorPath, summarizerPath string, err error) {
	p, e, s, _, _, _, err := writeEmbeddedPlanChains(contenoxDir, true)
	return p, e, s, err
}

// resolvePlanExplorerPath returns the on-disk path for chain-plan-explorer.json
// in the given contenoxDir. The file is written by [writeEmbeddedPlanChains];
// callers must ensure that has run (e.g. via [ensurePlanChains]) first.
func resolvePlanExplorerPath(contenoxDir string) string {
	return filepath.Join(contenoxDir, "chain-plan-explorer.json")
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
				"  The model output must be a JSON array of step-description strings, e.g.\n"+
				"  [\"First actionable step\", \"Second actionable step\"]",
			path,
		)
	}
	first := chain.Tasks[0]
	if first.Handler != taskengine.HandleChatCompletion {
		return fmt.Errorf(
			"planner chain %q: first task %q has handler %q, expected %q\n"+
				"  'contenox plan new' sends the goal as a user message and expects the chain to return\n"+
				"  a JSON array of step-description strings, e.g. [\"step 1\", \"step 2\"].\n"+
				"  The first task must be a chat_completion that generates this array.",
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

// validateSummarizerChain checks that a chain meets the contract required by plancompile.Compile
// for the summarizer slot. The summarizer chain must:
//   - Have at least one task.
//   - Reference the plan_summary hook (persist or fallback) somewhere in its tasks, so
//     the typed JSON actually gets persisted.
// Full structural validation happens inside plancompile.Compile (edges, cross-references).
func validateSummarizerChain(chain *taskengine.TaskChainDefinition, path string) error {
	if len(chain.Tasks) == 0 {
		return fmt.Errorf(
			"summarizer chain %q has no tasks\n"+
				"  The summarizer chain must have at least one chat_completion task and at least one\n"+
				"  hook task invoking plan_summary (tool_name: persist or fallback).",
			path,
		)
	}
	hasPersist := false
	hasFallback := false
	for _, task := range chain.Tasks {
		if task.Handler != taskengine.HandleHook || task.Hook == nil {
			continue
		}
		if strings.TrimSpace(task.Hook.Name) != "plan_summary" {
			continue
		}
		switch strings.TrimSpace(task.Hook.ToolName) {
		case "persist":
			hasPersist = true
		case "fallback":
			hasFallback = true
		}
	}
	if !hasPersist {
		return fmt.Errorf(
			"summarizer chain %q: no task invokes hook plan_summary tool_name=persist\n"+
				"  Without this, validated JSON summaries are never persisted to planstore.",
			path,
		)
	}
	if !hasFallback {
		return fmt.Errorf(
			"summarizer chain %q: no task invokes hook plan_summary tool_name=fallback\n"+
				"  Without this, a step whose summarizer output fails validation twice will leave the\n"+
				"  plan_step row with no summary at all — next step's handover would fall through to empty.",
			path,
		)
	}
	return nil
}

// validatePlanExplorerChain checks that a chain meets the contract required by
// 'contenox plan explore'. The explorer chain must:
//   - have at least one chat_completion task,
//   - have an execute_tool_calls task and a "tool-call" branch on the chat,
//   - close the agentic loop (same as executor),
//   - declare ONLY read-only hooks (typically local_fs); local_shell is rejected
//     because exploration is meant to be side-effect free.
func validatePlanExplorerChain(chain *taskengine.TaskChainDefinition, path string) error {
	if err := validateExecutorChain(chain, path); err != nil {
		return err
	}
	for _, task := range chain.Tasks {
		if task.Handler != taskengine.HandleChatCompletion || task.ExecuteConfig == nil {
			continue
		}
		for _, h := range task.ExecuteConfig.Hooks {
			if strings.TrimSpace(h) == "local_shell" {
				return fmt.Errorf(
					"explorer chain %q: task %q allowlists local_shell\n"+
						"  Exploration must be read-only — restrict hooks to local_fs (and other read-only tools).",
					path, task.ID,
				)
			}
			if strings.TrimSpace(h) == "*" {
				return fmt.Errorf(
					"explorer chain %q: task %q uses wildcard hook %q\n"+
						"  Exploration must be read-only — list only read-only hook names (e.g. \"local_fs\").",
					path, task.ID, h,
				)
			}
		}
	}
	return nil
}

// validateAgenticLoopClosure ensures each execute_tool_calls task eventually reaches its chat task
// (input_var): either directly (chain-contenox) or via one intermediate task (e.g. post-tool gate).
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
			if toolBranchReturnsToChat(strings.TrimSpace(b.Goto), chatID, chain) {
				loops = true
				break
			}
		}
		if !loops {
			return fmt.Errorf(
				"executor chain %q: agentic loop not closed — task %q (execute_tool_calls) must branch to %q\n"+
					"  or to a task that branches to %q (e.g. a post-tool gate), like chain-contenox run_tools → chat.",
				path, task.ID, chatID, chatID,
			)
		}
	}
	return nil
}

// toolBranchReturnsToChat is true if gotoID is the chat task or some task's branch targets chatID in one step.
func toolBranchReturnsToChat(gotoID, chatID string, chain *taskengine.TaskChainDefinition) bool {
	if gotoID == chatID {
		return true
	}
	for _, t := range chain.Tasks {
		if t.ID != gotoID {
			continue
		}
		for _, b := range t.Transition.Branches {
			if strings.TrimSpace(b.Goto) == chatID {
				return true
			}
		}
	}
	return false
}
