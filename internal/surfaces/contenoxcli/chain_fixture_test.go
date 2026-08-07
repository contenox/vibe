package contenoxcli

import (
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/require"
)

func TestUnit_BuiltinChains_SetThinkOnlyOnUserFacingChatTasks(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantThink   []string
		wantNoThink []string
	}{
		{name: "contenox", raw: initChain, wantThink: []string{"coding_chat", "coding_recovery", "contenox_chat", "recovery_chat", "summarise_failure"}, wantNoThink: []string{"classify_request", "coding_tools", "coding_recovery_tools", "run_tools", "recovery_tools"}},
		{name: "run", raw: initRunChain, wantThink: []string{"contenox_run", "recovery_run", "summarise_failure"}, wantNoThink: []string{"run_tools", "recovery_run_tools"}},
		{name: "acp", raw: initACPChain, wantThink: []string{"coding_chat", "coding_recovery", "acp_chat", "recovery_chat", "summarise_failure"}, wantNoThink: []string{"classify_request", "coding_tools", "coding_recovery_tools", "run_tools", "recovery_tools"}},
		{name: "acpx", raw: initACPXChain, wantThink: []string{"acp_chat", "recovery_chat", "summarise_failure"}, wantNoThink: []string{"run_tools", "recovery_tools"}},
		{name: "beam", raw: initBeamChain, wantThink: []string{"coding_chat", "coding_recovery", "acp_chat", "recovery_chat", "summarise_failure"}, wantNoThink: []string{"classify_request", "coding_tools", "coding_recovery_tools", "run_tools", "recovery_tools"}},
		{name: "compact", raw: initCompactChain, wantNoThink: []string{"compact_history"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var chain taskengine.TaskChainDefinition
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &chain))
			byID := make(map[string]taskengine.TaskDefinition)
			for _, task := range chain.Tasks {
				byID[task.ID] = task
			}
			for _, id := range tc.wantThink {
				task, ok := byID[id]
				require.True(t, ok, "task %s missing", id)
				require.NotNil(t, task.ExecuteConfig, "task %s execute_config", id)
				require.Equal(t, "{{var:think}}", task.ExecuteConfig.Think, "task %s think", id)
			}
			for _, id := range tc.wantNoThink {
				task, ok := byID[id]
				require.True(t, ok, "task %s missing", id)
				if task.ExecuteConfig != nil {
					require.Empty(t, task.ExecuteConfig.Think, "task %s should not set think", id)
				}
			}
		})
	}
}

// requireReadOnlyReviewLoop pins the property that makes the review branch a
// specialist rather than a differently-worded assistant: it cannot write.
//
// Every mutating tool is listed in hide_tools, which taskexec enforces at
// EXECUTION (isExecutionToolHidden), not merely by omitting it from the
// advertised list — so a model that asks for write_file anyway still cannot
// run it. Both halves of the loop must carry the same withholding, since a
// tool withheld only at the chat step would still execute at the tool step.
func requireReadOnlyReviewLoop(t *testing.T, byID map[string]taskengine.TaskDefinition) {
	t.Helper()
	mutating := []string{
		"local_fs.write_file",
		"local_fs.edit_file",
		"local_fs.sed",
		"git.git_add",
		"git.git_commit",
		"git.git_checkout_branch",
		"git.git_restore",
	}
	for _, id := range []string{"review_chat", "review_tools"} {
		task, ok := byID[id]
		require.True(t, ok, "chain has no %s task", id)
		require.NotNil(t, task.ExecuteConfig, "task %s execute_config", id)
		for _, tool := range mutating {
			require.Contains(t, task.ExecuteConfig.HideTools, tool,
				"task %s must withhold %s: a reviewer that can write is not read-only", id, tool)
		}
		require.Contains(t, task.ExecuteConfig.Tools, "!webtools",
			"task %s must exclude the network", id)
	}
	require.Equal(t, taskengine.HandleChatCompletion, byID["review_chat"].Handler)
	require.Equal(t, taskengine.HandleExecuteToolCalls, byID["review_tools"].Handler)
	require.Equal(t, "review_chat", byID["review_tools"].InputVar)
	require.Equal(t, "review_chat", branchGoto(t, byID["review_tools"], taskengine.OpDefault, "", "review_chat").Goto)
}

func TestUnit_ACPChain_RoutesToSimpleBoundedLoops(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(initACPChain), &chain))
	require.NotEmpty(t, chain.Tasks)
	require.Equal(t, "classify_request", chain.Tasks[0].ID)
	require.Len(t, chain.Tasks, 12)

	byID := make(map[string]taskengine.TaskDefinition)
	for _, task := range chain.Tasks {
		byID[task.ID] = task
	}

	classifier := byID["classify_request"]
	require.Equal(t, taskengine.HandleRoute, classifier.Handler)
	var routeLabels []string
	for _, branch := range classifier.Transition.Branches {
		if branch.Operator == taskengine.OpEquals {
			routeLabels = append(routeLabels, branch.When)
		}
	}
	require.ElementsMatch(t, []string{"coding_change", "general", "review_change"}, routeLabels)
	requireReadOnlyReviewLoop(t, byID)

	for _, oldID := range []string{
		"coding_inspect",
		"coding_inspect_tools",
		"coding_patch",
		"coding_patch_tools",
		"coding_verify",
		"coding_verify_tools",
		"coding_audit",
		"coding_audit_tools",
		"coding_audit_route",
		"coding_final",
		"coding_blocked",
		"verify",
		"revise",
	} {
		require.NotContains(t, byID, oldID)
	}

	requireLoop := func(chatID, toolsID, recoveryID string, toolBudget string) {
		chat := byID[chatID]
		require.Equal(t, taskengine.HandleChatCompletion, chat.Handler)
		require.NotNil(t, chat.ExecuteConfig, "task %s execute_config", chatID)
		require.Equal(t, []string{"*"}, chat.ExecuteConfig.Tools, "task %s tools", chatID)
		require.Equal(t, toolBudget, branchGoto(t, chat, taskengine.OpEdgeTraversedAtLeast, toolBudget, recoveryID).When)
		require.Equal(t, toolsID, branchGoto(t, chat, taskengine.OpEquals, taskengine.TransitionToolCall, toolsID).Goto)
		require.Equal(t, taskengine.TermEnd, branchGoto(t, chat, taskengine.OpDefault, "", taskengine.TermEnd).Goto)
		require.Equal(t, "262144", chat.ExecuteConfig.ToolsPolicies["local_fs"]["_max_read_bytes"])
		require.Equal(t, "131072", chat.ExecuteConfig.ToolsPolicies["local_fs"]["_max_output_bytes"])
		require.Equal(t, "1000", chat.ExecuteConfig.ToolsPolicies["local_fs"]["_max_grep_matches"])
		require.Equal(t, "262144", chat.ExecuteConfig.ToolsPolicies["webtools"]["_max_response_bytes"])

		tools := byID[toolsID]
		require.Equal(t, taskengine.HandleExecuteToolCalls, tools.Handler)
		require.Equal(t, chatID, tools.InputVar)
		require.NotNil(t, tools.ExecuteConfig, "task %s execute_config", toolsID)
		require.Equal(t, []string{"*"}, tools.ExecuteConfig.Tools, "task %s tools", toolsID)
		require.Equal(t, "262144", tools.ExecuteConfig.ToolsPolicies["local_fs"]["_max_read_bytes"])
		require.Equal(t, "131072", tools.ExecuteConfig.ToolsPolicies["local_fs"]["_max_output_bytes"])
	}

	requireLoop("coding_chat", "coding_tools", "coding_recovery", "60")
	requireLoop("acp_chat", "run_tools", "recovery_chat", "60")

	codingRecoveryTools := byID["coding_recovery_tools"]
	require.Equal(t, taskengine.HandleExecuteToolCalls, codingRecoveryTools.Handler)
	require.Equal(t, "coding_recovery", codingRecoveryTools.InputVar)
	require.Equal(t, []string{"*"}, codingRecoveryTools.ExecuteConfig.Tools)

	summary := byID["summarise_failure"]
	require.Equal(t, taskengine.HandleChatCompletion, summary.Handler)
	require.Equal(t, "previous_output", summary.InputVar)
	require.Empty(t, summary.ExecuteConfig.Tools)
}

func TestUnit_ContenoxChain_RoutesToSpecialistLoops(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(initChain), &chain))
	require.NotEmpty(t, chain.Tasks)
	require.Equal(t, "classify_request", chain.Tasks[0].ID)
	require.Len(t, chain.Tasks, 12)

	byID := make(map[string]taskengine.TaskDefinition)
	for _, task := range chain.Tasks {
		byID[task.ID] = task
	}

	classifier := byID["classify_request"]
	require.Equal(t, taskengine.HandleRoute, classifier.Handler)
	require.Equal(t, "coding_chat", branchGoto(t, classifier, taskengine.OpEquals, "coding_change", "coding_chat").Goto)
	require.Equal(t, "contenox_chat", branchGoto(t, classifier, taskengine.OpEquals, "general", "contenox_chat").Goto)
	require.Equal(t, "review_chat", branchGoto(t, classifier, taskengine.OpEquals, "review_change", "review_chat").Goto)
	require.Equal(t, "contenox_chat", branchGoto(t, classifier, taskengine.OpDefault, "", "contenox_chat").Goto)
	requireReadOnlyReviewLoop(t, byID)

	requireLoop := func(chatID, toolsID, recoveryID string, toolBudget string) {
		chat := byID[chatID]
		require.Equal(t, taskengine.HandleChatCompletion, chat.Handler)
		require.NotNil(t, chat.ExecuteConfig, "task %s execute_config", chatID)
		require.Equal(t, []string{"*"}, chat.ExecuteConfig.Tools, "task %s tools", chatID)
		require.Equal(t, toolBudget, branchGoto(t, chat, taskengine.OpEdgeTraversedAtLeast, toolBudget, recoveryID).When)
		require.Equal(t, toolsID, branchGoto(t, chat, taskengine.OpEquals, taskengine.TransitionToolCall, toolsID).Goto)
		require.Equal(t, taskengine.TermEnd, branchGoto(t, chat, taskengine.OpDefault, "", taskengine.TermEnd).Goto)

		tools := byID[toolsID]
		require.Equal(t, taskengine.HandleExecuteToolCalls, tools.Handler)
		require.Equal(t, chatID, tools.InputVar)
		require.NotNil(t, tools.ExecuteConfig, "task %s execute_config", toolsID)
		require.Equal(t, []string{"*"}, tools.ExecuteConfig.Tools, "task %s tools", toolsID)
		require.Contains(t, tools.ExecuteConfig.ToolsPolicies, "local_fs", "task %s", toolsID)
		require.Contains(t, tools.ExecuteConfig.ToolsPolicies, "webtools", "task %s", toolsID)
	}

	requireLoop("coding_chat", "coding_tools", "coding_recovery", "60")
	requireLoop("contenox_chat", "run_tools", "recovery_chat", "60")

	codingRecoveryTools := byID["coding_recovery_tools"]
	require.Equal(t, taskengine.HandleExecuteToolCalls, codingRecoveryTools.Handler)
	require.Equal(t, "coding_recovery", codingRecoveryTools.InputVar)
	require.Equal(t, []string{"*"}, codingRecoveryTools.ExecuteConfig.Tools)
	require.Equal(t, "8", branchGoto(t, byID["coding_recovery"], taskengine.OpEdgeTraversedAtLeast, "8", "summarise_failure").When)
	require.Equal(t, "8", branchGoto(t, byID["recovery_chat"], taskengine.OpEdgeTraversedAtLeast, "8", "summarise_failure").When)

	summary := byID["summarise_failure"]
	require.Equal(t, taskengine.HandleChatCompletion, summary.Handler)
	require.Equal(t, "previous_output", summary.InputVar)
	require.Empty(t, summary.ExecuteConfig.Tools)
}

func TestUnit_BuiltinRecoveryTasksUseConfiguredDefaultFallback(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ids  []string
	}{
		{name: "contenox", raw: initChain, ids: []string{"coding_recovery", "recovery_chat", "summarise_failure"}},
		{name: "run", raw: initRunChain, ids: []string{"recovery_run", "summarise_failure"}},
		{name: "acp", raw: initACPChain, ids: []string{"coding_recovery", "recovery_chat", "summarise_failure"}},
		{name: "acpx", raw: initACPXChain, ids: []string{"recovery_chat", "summarise_failure"}},
		{name: "beam", raw: initBeamChain, ids: []string{"coding_recovery", "recovery_chat", "summarise_failure"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var chain taskengine.TaskChainDefinition
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &chain))
			byID := make(map[string]taskengine.TaskDefinition)
			for _, task := range chain.Tasks {
				byID[task.ID] = task
			}
			for _, id := range tc.ids {
				task, ok := byID[id]
				require.True(t, ok, "task %s missing", id)
				require.NotNil(t, task.ExecuteConfig, "task %s execute_config", id)
				require.Equal(t, "{{var:alt_model|var:default_model}}", task.ExecuteConfig.Model, "task %s model", id)
				require.Equal(t, "{{var:alt_provider|var:default_provider}}", task.ExecuteConfig.Provider, "task %s provider", id)
			}
		})
	}
}

func TestUnit_BuiltinInteractiveChains_UseConservativeToolOutputCaps(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "contenox", raw: initChain},
		{name: "run", raw: initRunChain},
		{name: "acp", raw: initACPChain},
		{name: "acpx", raw: initACPXChain},
		{name: "beam", raw: initBeamChain},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var chain taskengine.TaskChainDefinition
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &chain))
			for _, task := range chain.Tasks {
				if task.ExecuteConfig == nil {
					continue
				}
				fsPolicy := task.ExecuteConfig.ToolsPolicies["local_fs"]
				if len(fsPolicy) > 0 {
					require.Equal(t, "262144", fsPolicy["_max_read_bytes"], "task %s", task.ID)
					require.Equal(t, "131072", fsPolicy["_max_output_bytes"], "task %s", task.ID)
					require.Equal(t, "1000", fsPolicy["_max_grep_matches"], "task %s", task.ID)
				}
				webPolicy := task.ExecuteConfig.ToolsPolicies["webtools"]
				if len(webPolicy) > 0 {
					require.Equal(t, "262144", webPolicy["_max_response_bytes"], "task %s", task.ID)
				}
			}
		})
	}
}

func TestUnit_BuiltinInteractiveChains_ScopeToolExecutionNodes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "contenox", raw: initChain},
		{name: "run", raw: initRunChain},
		{name: "acp", raw: initACPChain},
		{name: "acpx", raw: initACPXChain},
		{name: "beam", raw: initBeamChain},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var chain taskengine.TaskChainDefinition
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &chain))
			byID := map[string]taskengine.TaskDefinition{}
			for _, task := range chain.Tasks {
				byID[task.ID] = task
			}
			for _, task := range chain.Tasks {
				if task.Handler != taskengine.HandleExecuteToolCalls {
					continue
				}
				require.NotNil(t, task.ExecuteConfig, "task %s execute_config", task.ID)

				// The executor must offer EXACTLY the scope its chat node
				// offered. Asserting a literal ["*"] instead would forbid a
				// scoped specialist (the review loop drops the network; the
				// planner grants only mission tools) while still missing the
				// drift that actually matters: a tool withheld from the model
				// at the chat step but still runnable at the execute step.
				source, found := byID[task.InputVar]
				require.True(t, found, "task %s input_var %q names no task", task.ID, task.InputVar)
				require.NotNil(t, source.ExecuteConfig, "task %s source execute_config", source.ID)
				require.Equal(t, source.ExecuteConfig.Tools, task.ExecuteConfig.Tools,
					"task %s tools must match its %s scope", task.ID, source.ID)
				require.ElementsMatch(t, source.ExecuteConfig.HideTools, task.ExecuteConfig.HideTools,
					"task %s hide_tools must match its %s scope, or a withheld tool still runs", task.ID, source.ID)

				require.Contains(t, task.ExecuteConfig.ToolsPolicies, "local_fs", "task %s", task.ID)
				// webtools carries the response/body byte caps, so it is
				// required whenever the network is in scope — and meaningless
				// when it has been excluded.
				if slices.Contains(task.ExecuteConfig.Tools, "!webtools") {
					require.NotContains(t, task.ExecuteConfig.ToolsPolicies, "webtools",
						"task %s excludes webtools but still carries its policy", task.ID)
				} else {
					require.Contains(t, task.ExecuteConfig.ToolsPolicies, "webtools", "task %s", task.ID)
				}
			}
		})
	}
}

func TestUnit_BuiltinChains_LLMTasksIncludeDateMacro(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "contenox", raw: initChain},
		{name: "run", raw: initRunChain},
		{name: "compact", raw: initCompactChain},
		{name: "acp", raw: initACPChain},
		{name: "acpx", raw: initACPXChain},
		{name: "beam", raw: initBeamChain},
		{name: "fim", raw: initFIMChain},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var chain taskengine.TaskChainDefinition
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &chain))
			for _, task := range chain.Tasks {
				switch task.Handler {
				case taskengine.HandleChatCompletion, taskengine.HandleRoute:
					require.Contains(t, task.SystemInstruction, "{{date}}", "task %s", task.ID)
				}
			}
		})
	}
}

// TestUnit_PlannerChain_ProfileShape asserts the planner chain grants only mission tools, never execution tools, and its prompt retains the required discipline markers.
func TestUnit_PlannerChain_ProfileShape(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(initPlannerChain), &chain))

	require.Equal(t, "agent-planner", chain.ID, "the chain id is the discovered agent name")
	require.NotEmpty(t, chain.Tasks)

	forbidden := map[string]bool{"*": true, "local_shell": true, "local_fs": true, "webtools": true}
	sawMissionGrant := false
	for _, task := range chain.Tasks {
		if task.ExecuteConfig == nil || len(task.ExecuteConfig.Tools) == 0 {
			continue
		}
		for _, tool := range task.ExecuteConfig.Tools {
			require.Falsef(t, forbidden[tool], "task %s grants %q — the planner withholds execution tools", task.ID, tool)
		}
		require.Equal(t, []string{"mission"}, task.ExecuteConfig.Tools, "task %s grants only the mission tools", task.ID)
		sawMissionGrant = true
	}
	require.True(t, sawMissionGrant, "the planner must grant the mission tools somewhere")

	// These markers are the discipline the planner's prompt must keep; the runtime does not enforce them.
	var prompt string
	for _, task := range chain.Tasks {
		if task.ID == "plan_loop" {
			prompt = task.SystemInstruction
		}
	}
	require.NotEmpty(t, prompt, "the main planner loop carries the discipline prompt")
	for _, marker := range []string{
		"{{date}}",
		"FULL SNAPSHOT",           // maintain the plan via full snapshots
		"echoing the `id`",        // id carry-forward
		"in_progress at any time", // exactly one in_progress
		"pending to completed",    // no pending->completed jumps
		"explanation",             // explanation on every scope pivot
		"NEVER RESTATE THE PLAN",  // anti-echo
		"mission_report",          // report via the report tool
		"handover",                // typed handover
		"mission_finish",          // end with finish
		"not yet yours",           // sub-mission firing is a future slice
	} {
		require.Containsf(t, prompt, marker, "planner prompt is missing the %q discipline", marker)
	}
}

func branchGoto(t *testing.T, task taskengine.TaskDefinition, operator taskengine.OperatorTerm, when, gotoID string) taskengine.TransitionBranch {
	t.Helper()
	for _, branch := range task.Transition.Branches {
		if branch.Operator == operator && branch.When == when && branch.Goto == gotoID {
			return branch
		}
	}
	require.Failf(t, "missing branch", "task %s missing branch operator=%s when=%q goto=%q", task.ID, operator, when, gotoID)
	return taskengine.TransitionBranch{}
}

// TestUnit_BuiltinChains_ModelMacroFallbacksAlwaysSeeded asserts every model macro's fallback var is always seeded by both the CLI and ACP execution paths.
func TestUnit_BuiltinChains_ModelMacroFallbacksAlwaysSeeded(t *testing.T) {
	alwaysSeeded := map[string]bool{
		"model": true, "provider": true,
		"default_model": true, "default_provider": true,
	}
	chains := map[string]string{
		"contenox": initChain,
		"run":      initRunChain,
		"acp":      initACPChain,
		"acpx":     initACPXChain,
		"beam":     initBeamChain,
		"compact":  initCompactChain,
	}
	macroRe := regexp.MustCompile(`^\{\{var:([a-z_]+)(\|var:([a-z_]+))?\}\}$`)
	for name, raw := range chains {
		var chain taskengine.TaskChainDefinition
		require.NoError(t, json.Unmarshal([]byte(raw), &chain), name)
		for _, task := range chain.Tasks {
			if task.ExecuteConfig == nil || task.ExecuteConfig.Model == "" {
				continue
			}
			m := macroRe.FindStringSubmatch(task.ExecuteConfig.Model)
			require.NotNil(t, m, "%s/%s: unexpected model macro shape %q", name, task.ID, task.ExecuteConfig.Model)
			final := m[1]
			if m[3] != "" {
				final = m[3] // fallback var is the floor
			}
			require.True(t, alwaysSeeded[final],
				"%s/%s: model macro %q bottoms out in %q, which is not always seeded",
				name, task.ID, task.ExecuteConfig.Model, final)
		}
	}
}
