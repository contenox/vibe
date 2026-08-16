package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oracleSeededNames is the oracle's seeded set: one chain and the pinned
// envelope. One variant, not two — a dial with no units is worse than none.
var oracleSeededNames = []string{
	chainOracleDefaultFilename,
	"hitl-policy-oracle.json",
}

// seededOraclePaths is where init actually writes the oracle set. The chain
// goes under system/ with the rest of the machinery; the envelope stays at the
// top level, because the rules an agent runs under are meant to be read.
func seededOraclePaths(contenoxDir string) []string {
	return []string{
		filepath.Join(contenoxDir, SystemDirName, chainOracleDefaultFilename),
		filepath.Join(contenoxDir, "hitl-policy-oracle.json"),
	}
}

// TestUnit_InitGlobal_SeedsOracleSet proves RunGlobalInit (the home half of
// `contenox init` and the setup wizard) seeds the oracle set.
func TestUnit_InitGlobal_SeedsOracleSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var out bytes.Buffer
	require.NoError(t, RunGlobalInit(&out))
	for _, path := range seededOraclePaths(filepath.Join(home, ".contenox")) {
		require.FileExistsf(t, path, "%s must be seeded by global init", path)
	}
	require.NoFileExists(t, filepath.Join(home, ".contenox", "trigger-oracle-default.json"),
		"the oracle no longer rides a trigger; nothing seeds one")
}

// TestUnit_InitLocal_SeedsOracleSet proves `init --local` writes the same
// files into the workspace .contenox (shadow parity), and the doctor shadow
// set (initSystemFileNames) carries every one of them.
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
}

// TestUnit_InitRun_SeedsOracleSet proves the full `contenox init` path writes
// the oracle set to ~/.contenox like every other preset.
func TestUnit_InitRun_SeedsOracleSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), ".contenox")

	var out, errOut bytes.Buffer
	require.NoError(t, RunInit(&out, &errOut, false, false, "ollama", workspace, ""))
	for _, path := range seededOraclePaths(filepath.Join(home, ".contenox")) {
		require.FileExistsf(t, path, "%s must be seeded by contenox init", path)
	}
}

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

// TestUnit_EmbeddedOracleChain_AgenticLoopShape pins the chain to the shipped
// agent-chain loop stripped to one tool: chat → tool_call → execute_tool_calls
// → back, a deterministic verdict_state gate on the text branch, a recovery
// loop, edge budgets, and machine-contract prompts.
func TestUnit_EmbeddedOracleChain_AgenticLoopShape(t *testing.T) {
	name, content := chainOracleDefaultFilename, initOracleDefaultChain
	chain, tasks := oracleChainByID(t, content)
	require.NoError(t, taskengine.LintChain(&chain), "%s must pass the chain linter", name)

	for _, macro := range []string{"{{var:model}}", "{{var:provider}}", "{{var:think}}"} {
		assert.Truef(t, strings.Contains(content, macro),
			"%s must use the standard %s macro so the operator's defaults apply", name, macro)
	}
	assert.NotContains(t, content, "local_shell", "the oracle executes nothing: no shell tool anywhere in the chain")

	loop := tasks["oracle_loop"]
	require.NotNil(t, loop, "%s must have the main loop task", name)
	require.Equal(t, taskengine.HandleChatCompletion, loop.Handler)
	require.NotNil(t, loop.ExecuteConfig)
	assert.Equal(t, []string{oracletools.ToolsProviderName}, loop.ExecuteConfig.Tools,
		"the model holds exactly the oracle toolset, nothing else")
	assert.True(t, strings.HasPrefix(loop.SystemInstruction, "You are a task processing engine talking to other machines."),
		"machine-contract register, no coaching")
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
}

// TestUnit_EmbeddedOracleChain_TeachesBothVerdictSets pins the prompt to the
// contract the tool actually enforces: a permission ask takes approve/deny/wait
// and a question takes answer/wait, with guidance named as a denial's field.
func TestUnit_EmbeddedOracleChain_TeachesBothVerdictSets(t *testing.T) {
	_, tasks := oracleChainByID(t, initOracleDefaultChain)
	for _, id := range []string{"oracle_loop", "oracle_recovery"} {
		instr := tasks[id].SystemInstruction
		require.NotEmpty(t, instr, id)
		for _, want := range []string{
			"kind (", `"permission"`, `"attention"`,
			`"verdict":"approve"`, `"verdict":"deny"`, `"verdict":"answer"`, `"verdict":"wait"`,
			"guidance", "submit_verdict",
		} {
			assert.Containsf(t, instr, want, "task %s must teach %q", id, want)
		}
		assert.Contains(t, instr, "When unsure: wait.", "the default is always to leave it to a human")
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

	// The oracle never adjudicates its own asks, on either half.
	bounds, err := svc.AttentionBoundsFor(ctx, "hitl-policy-oracle.json")
	require.NoError(t, err)
	assert.False(t, bounds.AllowAgentAnswers)
	assert.False(t, bounds.AllowAgentApprovals)
}

// TestUnit_SeededOracleFiles_VetGreen proves `contenox vet` over the seeded
// oracle set, with and without the beta opt-in.
func TestUnit_SeededOracleFiles_VetGreen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var seedOut bytes.Buffer
	require.NoError(t, RunGlobalInit(&seedOut))
	contenoxDir := filepath.Join(home, ".contenox")

	files := seededOraclePaths(contenoxDir)

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

func oracleConfigDB(t *testing.T) (context.Context, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "oracle.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, runtimetypes.New(db.WithoutTransaction())
}

// TestUnit_OracleConfig_DefaultsAreOff pins the stance: with nothing
// configured there is no oracle at all, and every ask waits for a human.
func TestUnit_OracleConfig_DefaultsAreOff(t *testing.T) {
	ctx, store := oracleConfigDB(t)
	c := readOracleConfig(ctx, store)
	require.False(t, c.enabled(), "an unconfigured oracle is no oracle")
	require.False(t, c.approves, "ruling on tool calls is never the default")
	require.Equal(t, oracleDefaultPolicyName, c.policy, "the envelope has a default even while the oracle is off")
}

// TestUnit_OracleConfig_StoredValuesAndFlagOverrides pins the contenox
// convention: config sets the default, args override it per invocation.
func TestUnit_OracleConfig_StoredValuesAndFlagOverrides(t *testing.T) {
	ctx, store := oracleConfigDB(t)
	require.NoError(t, clikv.SetString(ctx, store, configKeyOracleChain, chainOracleDefaultFilename))
	require.NoError(t, clikv.SetString(ctx, store, configKeyOraclePolicy, "hitl-policy-custom.json"))
	require.NoError(t, clikv.SetString(ctx, store, configKeyOracleApprovesCall, "true"))

	stored := readOracleConfig(ctx, store)
	require.True(t, stored.enabled())
	require.Equal(t, chainOracleDefaultFilename, stored.chain)
	require.Equal(t, "hitl-policy-custom.json", stored.policy)
	require.True(t, stored.approves)

	newCmd := func() *cobra.Command {
		c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		registerOracleFlags(c)
		return c
	}

	t.Run("no flags keeps the stored values", func(t *testing.T) {
		got := resolveOracleConfig(ctx, store, newCmd())
		require.Equal(t, stored, got)
	})

	t.Run("flags win over config", func(t *testing.T) {
		cmd := newCmd()
		require.NoError(t, cmd.Flags().Set(flagOracleChain, "chain-oracle-other.json"))
		require.NoError(t, cmd.Flags().Set(flagOraclePolicy, "hitl-policy-flagged.json"))
		require.NoError(t, cmd.Flags().Set(flagOracleApproves, "false"))
		got := resolveOracleConfig(ctx, store, cmd)
		require.Equal(t, "chain-oracle-other.json", got.chain)
		require.Equal(t, "hitl-policy-flagged.json", got.policy)
		require.False(t, got.approves, "an explicit false must turn a configured true off")
	})

	t.Run("off disables a configured oracle for one run", func(t *testing.T) {
		cmd := newCmd()
		require.NoError(t, cmd.Flags().Set(flagOracleChain, "off"))
		got := resolveOracleConfig(ctx, store, cmd)
		require.False(t, got.enabled())
	})
}

// TestUnit_OracleConfigKeys_AreSettable pins that every key the oracle reads
// is one `contenox config set` accepts — an unlisted key is unreachable.
func TestUnit_OracleConfigKeys_AreSettable(t *testing.T) {
	for _, key := range []string{configKeyOracleChain, configKeyOraclePolicy, configKeyOracleApprovesCall} {
		_, ok := validConfigKeys[key]
		require.Truef(t, ok, "%s must be a documented config key", key)
	}
}

// TestUnit_OracleChainCandidates_TakesAnAgentNameOrAFilename pins the one
// vocabulary rule: default-oracle-chain accepts what default-mission-agent
// accepts, so an operator does not have to know which key wants a filename.
func TestUnit_OracleChainCandidates_TakesAnAgentNameOrAFilename(t *testing.T) {
	require.Equal(t, []string{"chain-oracle-default.json"},
		oracleChainCandidates("chain-oracle-default.json"), "a filename is used as given")

	require.Equal(t, []string{"chain-oracle-default", "chain-oracle-default.json"},
		oracleChainCandidates("chain-oracle-default"), "a chain id gains the extension")

	require.Equal(t, []string{"oracle-default", "oracle-default.json", "chain-oracle-default.json"},
		oracleChainCandidates("oracle-default"),
		"a bare agent name also reaches the chain a declared agent emits")

	require.Nil(t, oracleChainCandidates("  "), "blank names nothing")
}

// TestUnit_ACPPolicySource_SeesGeneratedEnvelopes pins the search path the ACP
// host evaluates subagents on.
func TestUnit_ACPPolicySource_SeesGeneratedEnvelopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	contenoxDir := filepath.Join(home, ".contenox")
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	require.NoError(t, os.MkdirAll(generated, 0o750))

	name := "hitl-policy-agent-researcher.json"
	require.NoError(t, os.WriteFile(filepath.Join(generated, name),
		[]byte(`{"default_action":"deny","rules":[]}`), 0o644))

	raw, err := acpPolicySource(contenoxDir).ReadPolicy(context.Background(), "", name)
	require.NoError(t, err, "a declared subagent's emitted envelope must be loadable by name")
	require.Contains(t, string(raw), `"deny"`)
}
