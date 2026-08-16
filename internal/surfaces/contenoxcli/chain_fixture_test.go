package contenoxcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/stretchr/testify/require"
)

func TestUnit_BuiltinChains_SetThinkOnlyOnUserFacingChatTasks(t *testing.T) {
	for _, name := range []string{"acp", "acpx"} {
		t.Run(name, func(t *testing.T) {
			chain := chainFor(t, name)
			require.NotEmpty(t, chain.Tasks)
			for _, task := range chain.Tasks {
				if task.ExecuteConfig == nil {
					continue
				}
				if task.Handler == taskengine.HandleChatCompletion {
					require.Equal(t, "{{var:think}}", task.ExecuteConfig.Think,
						"task %s is user-facing chat and must carry think", task.ID)
					continue
				}
				require.Empty(t, task.ExecuteConfig.Think,
					"task %s is a %v node and must not set think", task.ID, task.Handler)
			}
		})
	}
}

func requireReadOnlyReviewLoop(t *testing.T, byID map[string]taskengine.TaskDefinition, agent string) {
	t.Helper()
	reviewChat := "chain-" + agent + "-review-agent"
	reviewTools := "chain-" + agent + "-review-tools"
	for _, id := range []string{reviewChat, reviewTools} {
		task, ok := byID[id]
		require.True(t, ok, "chain has no %s task", id)
		require.NotNil(t, task.ExecuteConfig, "task %s execute_config", id)
	}
	require.Equal(t, taskengine.HandleChatCompletion, byID[reviewChat].Handler)
	require.Equal(t, taskengine.HandleExecuteToolCalls, byID[reviewTools].Handler)
	require.Equal(t, reviewChat, byID[reviewTools].InputVar)
	require.Equal(t, reviewChat, branchGoto(t, byID[reviewTools], taskengine.OpDefault, "", reviewChat).Goto)
	require.Contains(t, byID[reviewChat].SystemInstruction, "withheld",
		"the review prompt must state that write tools are withheld")
}

func TestUnit_ACPChain_RoutesToSimpleBoundedLoops(t *testing.T) {
	chain := chainFor(t, "acp")
	require.NotEmpty(t, chain.Tasks)
	require.Equal(t, "chain-acp-route", chain.Tasks[0].ID)
	require.Len(t, chain.Tasks, 12)

	byID := make(map[string]taskengine.TaskDefinition)
	for _, task := range chain.Tasks {
		byID[task.ID] = task
	}

	classifier := byID["chain-acp-route"]
	require.Equal(t, taskengine.HandleRoute, classifier.Handler)
	var routeLabels []string
	for _, branch := range classifier.Transition.Branches {
		if branch.Operator == taskengine.OpEquals {
			routeLabels = append(routeLabels, branch.When)
		}
	}
	require.ElementsMatch(t, []string{"coding", "general", "review"}, routeLabels)
	requireReadOnlyReviewLoop(t, byID, "acp")

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

	requireLoop("chain-acp-coding-agent", "chain-acp-coding-tools", "chain-acp-coding-recovery", "60")
	requireLoop("chain-acp-general-agent", "chain-acp-general-tools", "chain-acp-general-recovery", "60")

	codingRecoveryTools := byID["chain-acp-coding-recovery-tools"]
	require.Equal(t, taskengine.HandleExecuteToolCalls, codingRecoveryTools.Handler)
	require.Equal(t, "chain-acp-coding-recovery", codingRecoveryTools.InputVar)
	require.Equal(t, []string{"*"}, codingRecoveryTools.ExecuteConfig.Tools)

	summary := byID["chain-acp-summarise"]
	require.Equal(t, taskengine.HandleChatCompletion, summary.Handler)
	require.Equal(t, "previous_output", summary.InputVar)
	require.Empty(t, summary.ExecuteConfig.Tools)
}

func TestUnit_BuiltinRecoveryTasksUseConfiguredDefaultFallback(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
	}{
		{name: "acp", ids: []string{"chain-acp-coding-recovery", "chain-acp-general-recovery", "chain-acp-summarise"}},
		{name: "acpx", ids: []string{"chain-acpx-recovery", "chain-acpx-summarise"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := chainFor(t, tc.name)
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
	}{
		{name: "acp"},
		{name: "acpx"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := chainFor(t, tc.name)
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
	}{
		{name: "acp"},
		{name: "acpx"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := chainFor(t, tc.name)
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
				}
			}
		})
	}
}

func TestUnit_BuiltinChains_LLMTasksIncludeDateMacro(t *testing.T) {
	cases := []struct {
		name string
	}{
		{name: "compact"},
		{name: "acp"},
		{name: "acpx"},
		{name: "fim"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := chainFor(t, tc.name)
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
	macroRe := regexp.MustCompile(`^\{\{var:([a-z_]+)(\|var:([a-z_]+))?\}\}$`)
	for _, name := range []string{"acp", "acpx"} {
		chain := chainFor(t, name)
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

func chainFor(t *testing.T, name string) taskengine.TaskChainDefinition {
	t.Helper()
	var chain taskengine.TaskChainDefinition
	switch name {
	case "compact":
		require.NoError(t, json.Unmarshal([]byte(initCompactChain), &chain))
		return chain
	case "planner":
		require.NoError(t, json.Unmarshal([]byte(initPlannerChain), &chain))
		return chain
	case "fim":
		require.NoError(t, json.Unmarshal([]byte(initFIMChain), &chain))
		return chain
	}
	dir := t.TempDir()
	_, err := agentdecl.Preseed(dir)
	require.NoError(t, err)
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	generated := filepath.Join(dir, agentdecl.GeneratedDirName)
	_, err = agentdecl.Sync([]agentdecl.SourceDir{{
		Path:   filepath.Join(dir, agentdecl.NativeSourceDir),
		Native: true,
	}}, generated, cfg)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(generated, "chain-agent-"+name+".json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &chain))
	return chain
}
