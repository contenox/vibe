package contenoxcli

import (
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/stretchr/testify/require"
)

// TestUnit_BeamChain_PassesLintChain asserts the seeded beam chain passes the
// same load-time linter `contenox vet` runs, matching every other seeded chain.
func TestUnit_BeamChain_PassesLintChain(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(initBeamChain), &chain))
	require.NoError(t, taskengine.LintChain(&chain))
}

// TestUnit_BeamChain_MirrorsACPStructure asserts beam's chain is the acp
// chain's structure verbatim (task ids, handlers, transitions, tool grants,
// tools_policies) — only the system prompts differ, per the clone this chain
// is meant to be.
func TestUnit_BeamChain_MirrorsACPStructure(t *testing.T) {
	var acp, beam taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(initACPChain), &acp))
	require.NoError(t, json.Unmarshal([]byte(initBeamChain), &beam))

	require.Equal(t, len(acp.Tasks), len(beam.Tasks), "same number of tasks")
	require.Equal(t, acp.TokenLimit, beam.TokenLimit)

	acpByID := make(map[string]taskengine.TaskDefinition, len(acp.Tasks))
	for _, task := range acp.Tasks {
		acpByID[task.ID] = task
	}
	for _, bt := range beam.Tasks {
		at, ok := acpByID[bt.ID]
		require.Truef(t, ok, "beam task %s has no acp counterpart", bt.ID)
		require.Equal(t, at.Handler, bt.Handler, "task %s handler", bt.ID)
		require.Equal(t, at.InputVar, bt.InputVar, "task %s input_var", bt.ID)
		// Transitions mirror exactly. Beam used to raise the attended coding
		// loop's tool-round cap above ACP's on the grounds that the operator is
		// watching; both now carry the same ceiling, sized so only a
		// non-converging turn reaches it, so the surfaces no longer differ in
		// how much work one turn may do.
		require.Equal(t, at.Transition, bt.Transition, "task %s transition", bt.ID)
		if at.ExecuteConfig == nil || bt.ExecuteConfig == nil {
			require.Equal(t, at.ExecuteConfig == nil, bt.ExecuteConfig == nil, "task %s execute_config presence", bt.ID)
			continue
		}
		require.Equal(t, at.ExecuteConfig.Tools, bt.ExecuteConfig.Tools, "task %s tools", bt.ID)
		require.Equal(t, at.ExecuteConfig.ToolsPolicies, bt.ExecuteConfig.ToolsPolicies, "task %s tools_policies", bt.ID)
		require.Equal(t, at.ExecuteConfig.Model, bt.ExecuteConfig.Model, "task %s model", bt.ID)
		require.Equal(t, at.ExecuteConfig.Provider, bt.ExecuteConfig.Provider, "task %s provider", bt.ID)
		require.Equal(t, at.ExecuteConfig.MaxTokens, bt.ExecuteConfig.MaxTokens, "task %s max_tokens", bt.ID)
		// The prompt is the one thing allowed to differ. Tool-execution tasks
		// carry no system_instruction at all, and classify_request's never
		// mentioned the editor in the first place, so both are legitimately
		// unchanged; every user-facing chat/recovery prompt must be
		// rewritten for beam rather than copied verbatim.
		if bt.ID == "classify_request" || bt.SystemInstruction == "" {
			continue
		}
		require.NotEqual(t, at.SystemInstruction, bt.SystemInstruction, "task %s: beam must rewrite the prompt, not copy it verbatim", bt.ID)
	}
}

// TestUnit_BeamChain_CodingPromptStatesToolDiscipline pins the concrete
// tool-usage guidance the beam TUI coding loop's prompt must state: edit_file
// over write_file with byte-exact/unique old_string and replace_all for
// renames, the navigation tools, the no-approval shell verbs as bare argv
// commands, re-running the narrowest check after an edit, and adjusting a
// denied/asked call rather than abandoning the tool.
func TestUnit_BeamChain_CodingPromptStatesToolDiscipline(t *testing.T) {
	var chain taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal([]byte(initBeamChain), &chain))

	var prompt string
	for _, task := range chain.Tasks {
		if task.ID == "coding_chat" {
			prompt = task.SystemInstruction
		}
	}
	require.NotEmpty(t, prompt, "coding_chat must carry the TUI coding-loop prompt")

	for _, marker := range []string{
		"beam, the terminal UI",
		// edit_file over write_file
		"edit_file",
		"old_string",
		"byte-exact",
		"unique",
		"replace_all",
		"write_file only to create a file that does not exist yet",
		// navigation
		"grep",
		"recurses",
		"find_files",
		"** globs",
		"workspace_search",
		"go_definition",
		"go_references",
		// no-approval shell verbs, issued as bare argv
		"go build",
		"go test",
		"go vet",
		"go list",
		"gofmt -l",
		"gofmt -d",
		"ls, cat, head, tail, wc, pwd, grep, rg",
		"npm test",
		"vitest run",
		"jest",
		"pytest",
		"tsc --noEmit",
		"eslint",
		"ruff check",
		"mypy",
		"bare argv commands",
		"pipe, redirect, or command substitution",
		"chaining more than one of these verbs",
		// re-verify after an edit
		"re-run the narrowest build or test",
		// adjust on denial rather than giving up
		"WHEN A CALL IS DENIED OR ASKS",
		"adjust the call",
	} {
		require.Containsf(t, prompt, marker, "coding_chat prompt is missing %q", marker)
	}
}
