package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oracleSeededNames is the oracle attention driver's seeded set: the two
// chain variants and the pinned envelope. No trigger file — the driver
// mounts on `mission fire --oracle`, not on the event tier.
var oracleSeededNames = []string{
	chainOracleDefaultFilename,
	chainOracleConservativeFilename,
	"hitl-policy-oracle.json",
}

// TestUnit_InitGlobal_SeedsOracleSet proves RunGlobalInit (the home half of
// `contenox init` and the setup wizard) seeds the oracle set, and that the
// retired trigger file is no longer written.
func TestUnit_InitGlobal_SeedsOracleSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var out bytes.Buffer
	require.NoError(t, RunGlobalInit(&out))
	for _, name := range oracleSeededNames {
		require.FileExistsf(t, filepath.Join(home, ".contenox", name), "%s must be seeded by global init", name)
	}
	require.NoFileExists(t, filepath.Join(home, ".contenox", "trigger-oracle-default.json"),
		"the oracle no longer rides a trigger; nothing seeds one")
}

// TestUnit_InitLocal_SeedsOracleSet proves `init --local` writes the same
// files into the workspace .contenox (shadow parity), and the doctor shadow
// set (initSystemFileNames) carries every one of them and no trigger.
func TestUnit_InitLocal_SeedsOracleSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), ".contenox")

	var out bytes.Buffer
	require.NoError(t, RunLocalInit(&out, false, false, workspace, ""))
	for _, name := range oracleSeededNames {
		require.FileExistsf(t, filepath.Join(workspace, name), "%s must be seeded by init --local", name)
	}

	systemNames := initSystemFileNames()
	for _, name := range oracleSeededNames {
		require.Containsf(t, systemNames, name, "doctor's shadow set must include %s", name)
	}
	require.NotContains(t, systemNames, "trigger-oracle-default.json")
}

// TestUnit_InitRun_SeedsOracleSet proves the full `contenox init` path writes
// the oracle set to ~/.contenox like every other preset.
func TestUnit_InitRun_SeedsOracleSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), ".contenox")

	var out, errOut bytes.Buffer
	require.NoError(t, RunInit(&out, &errOut, false, false, "ollama", workspace, ""))
	for _, name := range oracleSeededNames {
		require.FileExistsf(t, filepath.Join(home, ".contenox", name), "%s must be seeded by contenox init", name)
	}
}

// oracleChainByID indexes a parsed chain's tasks.
func oracleChainByID(t *testing.T, raw string) (taskengine.TaskChainDefinition, map[string]*taskengine.TaskDefinition) {
	t.Helper()
	var chain taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(raw), &chain))
	byID := map[string]*taskengine.TaskDefinition{}
	for i := range chain.Tasks {
		byID[chain.Tasks[i].ID] = &chain.Tasks[i]
	}
	return chain, byID
}

// TestUnit_EmbeddedOracleChains_AgenticLoopShape pins both variants to the
// shipped agent-chain loop stripped to one tool: chat → tool_call →
// execute_tool_calls → back, a deterministic verdict_state gate on the text
// branch, a recovery loop, edge budgets, and machine-contract prompts. The
// oracle path carries NO shell tool of any kind.
func TestUnit_EmbeddedOracleChains_AgenticLoopShape(t *testing.T) {
	for name, content := range map[string]string{
		chainOracleDefaultFilename:      initOracleDefaultChain,
		chainOracleConservativeFilename: initOracleConservativeChain,
	} {
		t.Run(name, func(t *testing.T) {
			chain, tasks := oracleChainByID(t, content)
			require.NoError(t, taskengine.LintChain(&chain), "%s must pass the chain linter", name)

			for _, macro := range []string{"{{var:model}}", "{{var:provider}}", "{{var:think}}"} {
				assert.Truef(t, strings.Contains(content, macro),
					"%s must use the standard %s macro so the operator's defaults apply", name, macro)
			}
			assert.NotContains(t, content, "local_shell", "the oracle executes nothing: no shell tool anywhere in the chain")
			assert.NotContains(t, content, "from-verdict", "design #2's argv actuation is gone")

			loop := tasks["oracle_loop"]
			require.NotNil(t, loop, "%s must have the main loop task", name)
			require.Equal(t, taskengine.HandleChatCompletion, loop.Handler)
			require.NotNil(t, loop.ExecuteConfig)
			assert.Equal(t, []string{oracletools.ToolsProviderName}, loop.ExecuteConfig.Tools,
				"the model holds exactly the oracle toolset, nothing else")
			assert.True(t, strings.HasPrefix(loop.SystemInstruction, "You are a task processing engine talking to other machines."),
				"machine-contract register, no coaching")
			assert.Contains(t, loop.SystemInstruction, "submit_verdict")
			assert.Equal(t, "oracle_recovery", loop.Transition.OnFailure)

			// The loop's branch table, in order: tool budget → recovery, tool
			// call → execute, correction budget → end, text → corrective gate.
			branches := loop.Transition.Branches
			require.Len(t, branches, 4)
			assert.Equal(t, taskengine.OpEdgeTraversedAtLeast, branches[0].Operator)
			assert.Equal(t, "oracle_loop->oracle_tools", branches[0].Edge)
			assert.Equal(t, "oracle_recovery", branches[0].Goto)
			assert.Equal(t, taskengine.OpEquals, branches[1].Operator)
			assert.Equal(t, taskengine.TransitionToolCall, branches[1].When)
			assert.Equal(t, "oracle_tools", branches[1].Goto)
			assert.Equal(t, taskengine.OpEdgeTraversedAtLeast, branches[2].Operator)
			assert.Equal(t, "oracle_loop->oracle_correct", branches[2].Edge)
			assert.Equal(t, taskengine.TermEnd, branches[2].Goto, "correction budget spent ends WAIT-equivalent")
			assert.Equal(t, taskengine.OpDefault, branches[3].Operator)
			assert.Equal(t, "oracle_correct", branches[3].Goto, "chat text routes through the corrective gate")

			execTask := tasks["oracle_tools"]
			require.NotNil(t, execTask)
			require.Equal(t, taskengine.HandleExecuteToolCalls, execTask.Handler)
			assert.Equal(t, "oracle_loop", execTask.InputVar)
			assert.Equal(t, []string{oracletools.ToolsProviderName}, execTask.ExecuteConfig.Tools)
			assert.Equal(t, "oracle_loop", execTask.Transition.Branches[0].Goto, "tool results loop back to the model")

			gate := tasks["oracle_correct"]
			require.NotNil(t, gate)
			require.Equal(t, taskengine.HandleTools, gate.Handler)
			require.NotNil(t, gate.Tools)
			assert.Equal(t, oracletools.ToolsProviderName, gate.Tools.Name)
			assert.Equal(t, oracletools.ToolNameVerdictState, gate.Tools.ToolName)
			assert.Contains(t, gate.OutputTemplate, "settled")
			require.Len(t, gate.Transition.Branches, 2)
			assert.Equal(t, taskengine.OpEquals, gate.Transition.Branches[0].Operator)
			assert.Equal(t, "settled", gate.Transition.Branches[0].When)
			assert.Equal(t, taskengine.TermEnd, gate.Transition.Branches[0].Goto, "a settled contract ends the chain")
			assert.Equal(t, "oracle_loop", gate.Transition.Branches[1].Goto, "an open contract's correction retries")

			recovery := tasks["oracle_recovery"]
			require.NotNil(t, recovery)
			require.Equal(t, taskengine.HandleChatCompletion, recovery.Handler)
			assert.Equal(t, []string{oracletools.ToolsProviderName}, recovery.ExecuteConfig.Tools)
			assert.Contains(t, recovery.SystemInstruction, "{{edge_count:oracle_loop->oracle_tools}}",
				"the recovery budget line uses the shipped edge-count macros")
			rb := recovery.Transition.Branches
			require.Len(t, rb, 3)
			assert.Equal(t, taskengine.OpEdgeTraversedAtLeast, rb[0].Operator)
			assert.Equal(t, "oracle_recovery->oracle_recovery_tools", rb[0].Edge)
			assert.Equal(t, taskengine.TermEnd, rb[0].Goto)
			assert.Equal(t, taskengine.TransitionToolCall, rb[1].When)
			assert.Equal(t, "oracle_recovery_tools", rb[1].Goto)
			assert.Equal(t, taskengine.TermEnd, rb[2].Goto, "recovery text ends WAIT-equivalent")

			recoveryTools := tasks["oracle_recovery_tools"]
			require.NotNil(t, recoveryTools)
			require.Equal(t, taskengine.HandleExecuteToolCalls, recoveryTools.Handler)
			assert.Equal(t, "oracle_recovery", recoveryTools.Transition.Branches[0].Goto)
		})
	}
}

// TestUnit_EmbeddedOracleChains_VariantsDifferOnlyInDecision pins the two
// variants as one stripped template: identical topology, the DECISION
// paragraph the only judgment difference.
func TestUnit_EmbeddedOracleChains_VariantsDifferOnlyInDecision(t *testing.T) {
	_, def := oracleChainByID(t, initOracleDefaultChain)
	_, con := oracleChainByID(t, initOracleConservativeChain)

	assert.Contains(t, def["oracle_loop"].SystemInstruction, "together with the ask's summary and detail")
	assert.Contains(t, con["oracle_loop"].SystemInstruction, "stated intent alone")

	// Outside the DECISION paragraph the instructions are identical.
	strip := func(s string) string {
		lines := strings.Split(s, "\n")
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			if strings.HasPrefix(l, "DECISION:") {
				continue
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	for _, id := range []string{"oracle_loop", "oracle_recovery"} {
		assert.Equal(t, strip(def[id].SystemInstruction), strip(con[id].SystemInstruction),
			"task %s: the variants share one contract modulo DECISION", id)
	}
}

// TestUnit_OraclePolicy_AllowsExactlyTheOracleTools proves the envelope's
// whole capability surface: allow oracle.submit_verdict and
// oracle.verdict_state, deny everything else — and, per the PATH-resolution
// hazard, no shell tool and no command/prefix rule of any kind exists in it.
func TestUnit_OraclePolicy_AllowsExactlyTheOracleTools(t *testing.T) {
	t.Parallel()
	assert.NotContains(t, hitlPolicyOracle, "command_prefix_allowlist",
		"a prefix allowlist pins a NAME and PATH decides what it resolves to — banned from the oracle envelope")
	assert.NotContains(t, hitlPolicyOracle, "local_shell", "no shell rule of any kind")

	svc := seededPolicyService(t, "hitl-policy-oracle.json", hitlPolicyOracle)
	ctx := context.Background()

	for _, tool := range []string{oracletools.ToolNameSubmitVerdict, oracletools.ToolNameVerdictState} {
		r, err := svc.Evaluate(ctx, oracletools.ToolsProviderName, tool, map[string]any{"verdict": "wait", "askId": "ask-1"})
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionAllow, r.Action, "oracle.%s is the granted surface", tool)
	}

	denied := []struct{ tools, tool string }{
		{"local_shell", "local_shell"},
		{"local_fs", "read_file"},
		{"local_fs", "write_file"},
		{"webtools", "web_get"},
		{"git", "git_commit"},
		{oracletools.ToolsProviderName, "imagined_tool"},
	}
	for _, d := range denied {
		r, err := svc.Evaluate(ctx, d.tools, d.tool, map[string]any{"path": "x"})
		require.NoError(t, err)
		assert.Equalf(t, hitlservice.ActionDeny, r.Action, "%s.%s must fall to default deny — nobody watches this chain to approve", d.tools, d.tool)
	}

	// Attention stays human-only: the oracle never adjudicates its own asks.
	bounds, err := svc.AttentionBoundsFor(ctx, "hitl-policy-oracle.json")
	require.NoError(t, err)
	assert.False(t, bounds.AllowAgentAnswers)
}

// TestUnit_SeededOracleFiles_VetGreen proves `contenox vet` over the seeded
// oracle set, with and without the beta opt-in — chains and the policy are
// stable-class files, so both runs pass clean.
func TestUnit_SeededOracleFiles_VetGreen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var seedOut bytes.Buffer
	require.NoError(t, RunGlobalInit(&seedOut))
	contenoxDir := filepath.Join(home, ".contenox")

	var files []string
	for _, name := range oracleSeededNames {
		files = append(files, filepath.Join(contenoxDir, name))
	}

	var beta strings.Builder
	require.Equal(t, 0, runVetOnFiles(&beta, files, vetOpts{triggers: true, contenoxDir: contenoxDir}),
		"every seeded oracle file must vet green under opt-in-beta:\n%s", beta.String())
	assert.NotContains(t, beta.String(), "WARN", "the oracle set must vet without diagnostics")

	var stable strings.Builder
	require.Equal(t, 0, runVetOnFiles(&stable, files, vetOpts{}),
		"a stable vet run must not fail on the seeded oracle files:\n%s", stable.String())
	assert.NotContains(t, stable.String(), "FAIL")
	assert.NotContains(t, stable.String(), "WARN")
}

// TestUnit_MissionFireOracleFlag_BetaGated pins the flag's visibility gate:
// unregistered (absent, refused as unknown) on a stable invocation,
// registered under opt-in-beta — and oracleFlagSet reads it nil-safely.
func TestUnit_MissionFireOracleFlag_BetaGated(t *testing.T) {
	registerMissionFireFlags(false)
	t.Cleanup(func() { registerMissionFireFlags(false) })
	require.Nil(t, missionFireCmd.Flags().Lookup(oracleFlagName),
		"without opt-in-beta the flag does not exist at all")
	require.False(t, oracleFlagSet(missionFireCmd), "unregistered reads as off, never panics")
	require.NotNil(t, missionFireCmd.Flags().Lookup("policy"), "the stable flags survive the reset")

	registerMissionFireFlags(true)
	f := missionFireCmd.Flags().Lookup(oracleFlagName)
	require.NotNil(t, f, "opt-in-beta registers --oracle")
	require.False(t, oracleFlagSet(missionFireCmd), "registered but not given reads as off")
	require.NoError(t, missionFireCmd.Flags().Set(oracleFlagName, "true"))
	require.True(t, oracleFlagSet(missionFireCmd))
}
