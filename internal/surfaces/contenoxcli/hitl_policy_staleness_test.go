package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/stretchr/testify/require"
)

// shippedPreset returns the embedded content of a preset by name.
func shippedPreset(t *testing.T, name string) string {
	t.Helper()
	for _, p := range HITLPolicyPresets {
		if p.Name == name {
			return p.Content
		}
	}
	t.Fatalf("no embedded preset named %q", name)
	return ""
}

// TestUnit_MissingPolicyToolsets is the detector's own contract. The claim it
// makes is narrow on purpose: "this envelope has no rule for that toolset AT
// ALL" is evidence the file predates the toolset; anything weaker is evidence of
// nothing and must never produce a warning about someone's security boundary.
func TestUnit_MissingPolicyToolsets(t *testing.T) {
	t.Parallel()

	shipped := `{
		"default_action": "approve",
		"rules": [
			{"tools": "local_fs", "tool": "read_file", "action": "allow"},
			{"tools": "gointel", "tool": "go_symbol", "action": "allow"},
			{"tools": "workspace", "tool": "workspace_search", "action": "allow"}
		]
	}`

	t.Run("a toolset the file never mentions is detected", func(t *testing.T) {
		t.Parallel()
		onDisk := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
		require.Equal(t, []string{"gointel", "workspace"}, missingPolicyToolsets([]byte(shipped), []byte(onDisk)))
	})

	t.Run("reordered or reworded rules are not staleness", func(t *testing.T) {
		t.Parallel()
		// Same three toolsets, different order, different tool names, an extra
		// condition, a tightened action: all of it is the operator's business.
		onDisk := `{
			"default_action": "deny",
			"rules": [
				{"tools": "workspace", "tool": "*", "action": "approve"},
				{"tools": "gointel", "tool": "go_symbol", "action": "allow", "when": [{"key":"path","op":"prefix","value":"internal/"}]},
				{"tools": "local_fs", "tool": "read_file", "action": "allow"},
				{"tools": "some_local_mcp", "tool": "*", "action": "deny"}
			]
		}`
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk)))
	})

	t.Run("a deliberate deny is a decision, never a missing rule", func(t *testing.T) {
		t.Parallel()
		onDisk := `{
			"default_action": "approve",
			"rules": [
				{"tools": "local_fs", "tool": "read_file", "action": "allow"},
				{"tools": "gointel", "action": "deny"},
				{"tools": "workspace", "action": "deny"}
			]
		}`
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk)),
			"an operator who denied a toolset must never be told to re-add the shipped rule")
	})

	t.Run("a catch-all rule means nothing falls through", func(t *testing.T) {
		t.Parallel()
		for _, rule := range []string{
			`{"tools": "*", "tool": "*", "action": "approve"}`,
			`{"tools": "*", "action": "deny"}`,
			`{"action": "approve"}`,
		} {
			onDisk := `{"default_action":"approve","rules":[` + rule + `]}`
			require.Emptyf(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk)),
				"catch-all rule %s matches every toolset, so nothing is missing", rule)
		}
	})

	t.Run("a wildcard toolset pinned to one tool covers only that tool", func(t *testing.T) {
		t.Parallel()
		onDisk := `{"default_action":"approve","rules":[{"tools":"*","tool":"read_file","action":"allow"}]}`
		require.Equal(t, []string{"gointel", "local_fs", "workspace"},
			missingPolicyToolsets([]byte(shipped), []byte(onDisk)))
	})

	t.Run("an unreadable file makes no claim at all", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(`{ not json`)))
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(`{"rules": {"weird": true}}`)))
		// A rule of an unexpected shape could be the rule that covers a toolset,
		// so the whole file makes no claim rather than a wrong one.
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(`{"rules":[{"tools":["a","b"],"action":"allow"}]}`)))
	})

	t.Run("this build's own preset is never stale against itself", func(t *testing.T) {
		t.Parallel()
		for _, p := range HITLPolicyPresets {
			require.Emptyf(t, missingPolicyToolsets([]byte(p.Content), []byte(p.Content)),
				"%s reported itself stale", p.Name)
		}
	})
}

// TestUnit_StalePolicyPresets_PreStateFileInstall walks the shape that actually
// shipped the bug: an install whose ~/.contenox predates gointel/goja/jq/
// workspace and has no provenance record, so the upgrade cannot touch it.
func TestUnit_StalePolicyPresets_PreStateFileInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A previous build's default envelope: local_fs + git only.
	previous := `{"default_action":"approve","rules":[
		{"tools":"local_fs","tool":"read_file","action":"allow"},
		{"tools":"git","tool":"git_status","action":"allow"}
	]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))
	require.NoFileExists(t, filepath.Join(dir, presetStateFile))

	// The seeder holds the file back (it cannot prove it is untouched)...
	stale, err := upgradeEmbeddedHITLPolicies(dir, false)
	require.NoError(t, err)
	require.Contains(t, stale, "hitl-policy-default.json")

	// ...and THIS is what the operator is told instead: the toolsets, not the file hash.
	detected := stalePolicyPresets([]string{dir})
	require.Len(t, detected, 1, "only the file that predates a toolset is named: %#v", detected)
	require.Equal(t, "hitl-policy-default.json", detected[0].Name)
	require.Equal(t, filepath.Join(dir, "hitl-policy-default.json"), detected[0].Path)
	require.Equal(t, "approve", detected[0].DefaultAction)
	require.Subset(t, detected[0].Toolsets, []string{"gointel", "goja", "jq", "workspace"},
		"the toolsets that shipped today must all be named")
	require.NotContains(t, detected[0].Toolsets, "local_fs")
	require.NotContains(t, detected[0].Toolsets, "git")

	notice := stalePolicyNotice("hitl-policy-default.json", []string{dir})
	require.Contains(t, notice, "hitl-policy-default.json")
	require.Contains(t, notice, "workspace")
	require.Contains(t, notice, "stops for approval")
	require.Contains(t, notice, RefreshPoliciesCommand)
	require.NotContains(t, notice, "\n", "the startup notice is one line, not a wall")
}

// TestUnit_StalePolicyNotice_StopsAfterRefresh is the anti-noise guarantee: the
// line appears until the envelope has the rules, by either route, and then never
// again.
func TestUnit_StalePolicyNotice_StopsAfterRefresh(t *testing.T) {
	t.Parallel()
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`

	t.Run("the refresh verb silences it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))
		require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}))

		// What `contenox init --refresh-policies` does.
		require.NoError(t, writeEmbeddedHITLPolicies(dir, true))

		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}))
		require.Empty(t, stalePolicyPresets([]string{dir}))
		// And it stays silent on the next run, without rewriting anything.
		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stale)
		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}))
	})

	t.Run("the operator adding the rules by hand silences it too", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))
		require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}))

		// Their own envelope, their own verdicts — some allow, some deny.
		var b strings.Builder
		b.WriteString(`{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}`)
		for _, toolset := range stalePolicyPresets([]string{dir})[0].Toolsets {
			b.WriteString(`,{"tools":"` + toolset + `","action":"deny"}`)
		}
		b.WriteString(`]}`)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(b.String()), 0o644))

		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}),
			"the notice must stop once the envelope rules on the toolsets, whatever it decides")
	})

	t.Run("a fresh install never sees it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stalePolicyPresets([]string{dir}))
		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}))
	})

	t.Run("a preset that is not on disk is not stale", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.Empty(t, stalePolicyPresets([]string{dir}), "an empty dir gets this build's presets written to it")
		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}))
		require.Empty(t, stalePolicyNotice("not-a-shipped-preset.json", []string{dir}))
	})
}

// TestUnit_StalePolicyPresets_ReadsTheFileTheLoaderWouldRead pins the search
// order: the engine's policy source takes the first match along the path, so a
// current preset in the workspace dir must not be judged by a stale one in
// $HOME (or the reverse).
func TestUnit_StalePolicyPresets_ReadsTheFileTheLoaderWouldRead(t *testing.T) {
	t.Parallel()
	workspace, home := t.TempDir(), t.TempDir()
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(home, "hitl-policy-default.json"), []byte(previous), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "hitl-policy-default.json"),
		[]byte(shippedPreset(t, "hitl-policy-default.json")), 0o644))

	require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{workspace, home}),
		"the workspace copy wins, and it is current")
	require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{home, workspace}),
		"with the stale copy first, that is the one the loader reads")
}

// TestUnit_DoctorStalePolicyWarning covers the doctor surface end to end: the
// warning is rendered, it names the toolsets and the refresh verb, and it never
// makes doctor report a blocked runtime.
func TestUnit_DoctorStalePolicyWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))

	res := setupcheck.Result{DefaultModel: "qwen2.5:7b", DefaultProvider: "ollama"}
	res = setupcheck.AddStalePolicyPresetIssue(res, stalePolicyPresetIssues([]string{dir}), RefreshPoliciesCommand)

	var out strings.Builder
	printDoctorText(&out, res)
	text := out.String()
	require.Contains(t, text, "[warning]")
	require.Contains(t, text, "hitl-policy-default.json")
	require.Contains(t, text, "workspace")
	require.Contains(t, text, RefreshPoliciesCommand)
	require.NotContains(t, text, "All checks passed")

	require.Empty(t, res.BlockingIssues(), "a stale envelope must never block")
	require.True(t, res.Ready())

	// And with a current envelope doctor stays quiet.
	require.NoError(t, writeEmbeddedHITLPolicies(dir, true))
	clean := setupcheck.AddStalePolicyPresetIssue(
		setupcheck.Result{DefaultModel: "qwen2.5:7b", DefaultProvider: "ollama"},
		stalePolicyPresetIssues([]string{dir}), RefreshPoliciesCommand)
	var cleanOut strings.Builder
	printDoctorText(&cleanOut, clean)
	require.Contains(t, cleanOut.String(), "All checks passed.")
}

// TestUnit_PolicyDirs mirrors hitlPolicySource's search path, and collapses the
// duplicate beam produces by resolving its primary dir to $HOME/.contenox.
func TestUnit_PolicyDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".contenox")

	require.Equal(t, []string{"/work/.contenox", globalDir}, policyDirs("/work/.contenox"))
	require.Equal(t, []string{globalDir}, policyDirs(globalDir), "beam resolves both to ~/.contenox")
	require.Equal(t, []string{globalDir}, policyDirs(""))
}
