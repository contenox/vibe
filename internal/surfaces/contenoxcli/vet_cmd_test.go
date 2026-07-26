package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_Vet_IsAReservedSubcommand: without the reservation, `contenox vet`
// would be silently injected as chat input.
func TestUnit_Vet_IsAReservedSubcommand(t *testing.T) {
	require.True(t, reservedSubcommands["vet"], `"vet" must be reserved so it dispatches as a subcommand`)
}

// TestUnit_Vet_AllInRepoChainsAndPoliciesPass is the S4 gate: every chain and
// hitl-policy file the repo ships must pass its own vet. A failure here means
// either the linter grew a false positive or a shipped file carries a real
// latent bug — fix whichever is actually wrong, never weaken the check.
func TestUnit_Vet_AllInRepoChainsAndPoliciesPass(t *testing.T) {
	var files []string
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "chain-") || strings.HasPrefix(name, "agent-") || strings.HasPrefix(name, "hitl-policy-") {
			if strings.HasSuffix(name, ".json") {
				files = append(files, name)
			}
		}
	}
	require.NotEmpty(t, files, "expected shipped chain/policy fixtures beside this test")

	if examples, err := filepath.Glob(filepath.Join("..", "..", "..", "examples", "*.json")); err == nil {
		files = append(files, examples...)
	}

	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			vetted, err := vetOneFile(path)
			require.True(t, vetted, "shipped file %s must classify as chain or envelope", path)
			require.NoError(t, err)
		})
	}
}

func TestUnit_Vet_ClassifiesByContentThenName(t *testing.T) {
	require.Equal(t, vetKindChain, classifyVetFile("x.json", []byte(`{"id":"c","tasks":[]}`)))
	require.Equal(t, vetKindEnvelope, classifyVetFile("x.json", []byte(`{"rules":[]}`)))
	require.Equal(t, vetKindEnvelope, classifyVetFile("x.json", []byte(`{"default_action":"allow"}`)))
	require.Equal(t, vetKindEnvelope, classifyVetFile("hitl-policy-custom.json", []byte(`{}`)))
	require.Equal(t, vetKindSkip, classifyVetFile("workspace.json", []byte(`{"id":"w"}`)))
	require.Equal(t, vetKindSkip, classifyVetFile("notes.json", []byte(`[1,2,3]`)))
}

// TestUnit_Vet_ReportsPerFileAndCountsFailures drives the vet verb's engine
// over a mixed directory: passing chain, failing chain, failing envelope,
// and a skipped bystander file.
func TestUnit_Vet_ReportsPerFileAndCountsFailures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	good := write("good-chain.json", `{
		"id": "good",
		"tasks": [{"id": "reply", "handler": "noop", "transition": {"branches": [{"operator": "default", "when": "", "goto": "end"}]}}]
	}`)
	badChain := write("bad-chain.json", `{
		"id": "bad",
		"tasks": [
			{"id": "extract", "handler": "noop", "prompt_template": "{{.input}}",
			 "transition": {"branches": [{"operator": "default", "when": "", "goto": "summarize"}]}},
			{"id": "summarize", "handler": "execute_tool_calls", "transition": {"branches": []}}
		]
	}`)
	badPolicy := write("hitl-policy-broken.json", `{"rules": [{"tools": "local_*", "tool": "*", "action": "permit"}]}`)
	skipped := write("random.json", `{"hello": "world"}`)

	var out strings.Builder
	failed := runVetOnFiles(&out, []string{good, badChain, badPolicy, skipped})
	require.Equal(t, 2, failed)

	report := out.String()
	require.Contains(t, report, "ok   "+good)
	require.Contains(t, report, "FAIL "+badChain)
	// The teaching error names both endpoints of the impossible edge.
	require.Contains(t, report, "task[summarize] handler execute_tool_calls cannot accept input from task[extract] (produces string; accepts chat_history)")
	require.Contains(t, report, "FAIL "+badPolicy)
	require.Contains(t, report, `unknown action "permit"`)
	require.Contains(t, report, `tools "local_*" can never match`)
	require.Contains(t, report, "skip "+skipped)
}

// TestUnit_Vet_CollectExpandsDirectoriesRecursively pins the path argument
// semantics: a file is itself, a directory is every .json under it.
func TestUnit_Vet_CollectExpandsDirectoriesRecursively(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(sub, "b.json")
	require.NoError(t, os.WriteFile(a, []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(b, []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte(`x`), 0o600))

	files, err := collectVetFiles(dir)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{a, b}, files)

	single, err := collectVetFiles(a)
	require.NoError(t, err)
	require.Equal(t, []string{a}, single)

	_, err = collectVetFiles(filepath.Join(dir, "missing.json"))
	require.Error(t, err)
}
