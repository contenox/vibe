package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/setupcheck"
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

// TestUnit_MissingPolicyToolsets asserts a toolset counts as missing only when the envelope has no rule for it at all — nothing weaker triggers a warning.
func TestUnit_MissingPolicyToolsets(t *testing.T) {
	t.Parallel()

	shipped := `{
		"default_action": "approve",
		"rules": [
			{"tools": "local_fs", "tool": "read_file", "action": "allow"},
			{"tools": "git", "tool": "git_status", "action": "allow"},
			{"tools": "workspace", "tool": "workspace_search", "action": "allow"}
		]
	}`

	t.Run("a toolset the file never mentions is detected", func(t *testing.T) {
		t.Parallel()
		onDisk := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
		require.Equal(t, []string{"git", "workspace"}, missingPolicyToolsets([]byte(shipped), []byte(onDisk), nil))
	})

	t.Run("reordered or reworded rules are not staleness", func(t *testing.T) {
		t.Parallel()
		// Reordering, renaming tools, adding conditions, tightening actions:
		// all the operator's business, none of it staleness.
		onDisk := `{
			"default_action": "deny",
			"rules": [
				{"tools": "workspace", "tool": "*", "action": "approve"},
				{"tools": "git", "tool": "git_status", "action": "allow", "when": [{"key":"path","op":"prefix","value":"internal/"}]},
				{"tools": "local_fs", "tool": "read_file", "action": "allow"},
				{"tools": "some_local_mcp", "tool": "*", "action": "deny"}
			]
		}`
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk), nil))
	})

	t.Run("a deliberate deny is a decision, never a missing rule", func(t *testing.T) {
		t.Parallel()
		onDisk := `{
			"default_action": "approve",
			"rules": [
				{"tools": "local_fs", "tool": "read_file", "action": "allow"},
				{"tools": "git", "action": "deny"},
				{"tools": "workspace", "action": "deny"}
			]
		}`
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk), nil),
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
			require.Emptyf(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk), nil),
				"catch-all rule %s matches every toolset, so nothing is missing", rule)
		}
	})

	t.Run("a wildcard toolset pinned to one tool covers only that tool", func(t *testing.T) {
		t.Parallel()
		onDisk := `{"default_action":"approve","rules":[{"tools":"*","tool":"read_file","action":"allow"}]}`
		require.Equal(t, []string{"git", "local_fs", "workspace"},
			missingPolicyToolsets([]byte(shipped), []byte(onDisk), nil))
	})

	t.Run("an unreadable file makes no claim at all", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(`{ not json`), nil))
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(`{"rules": {"weird": true}}`), nil))
		// A rule of an unexpected shape could be the rule that covers a toolset,
		// so the whole file makes no claim rather than a wrong one.
		require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(`{"rules":[{"tools":["a","b"],"action":"allow"}]}`), nil))
	})

	t.Run("this build's own preset is never stale against itself", func(t *testing.T) {
		t.Parallel()
		for _, p := range HITLPolicyPresets {
			require.Emptyf(t, missingPolicyToolsets([]byte(p.Content), []byte(p.Content), nil),
				"%s reported itself stale", p.Name)
		}
	})
}

// TestUnit_StalePolicyPresets_PreStateFileInstall asserts an install predating goja/workspace, with no provenance record, is detected as stale and left untouched.
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
	detected := stalePolicyPresets([]string{dir}, nil)
	require.Len(t, detected, 1, "only the file that predates a toolset is named: %#v", detected)
	require.Equal(t, "hitl-policy-default.json", detected[0].Name)
	require.Equal(t, filepath.Join(dir, "hitl-policy-default.json"), detected[0].Path)
	require.Equal(t, "approve", detected[0].DefaultAction)
	require.Subset(t, detected[0].Toolsets, []string{"goja", "local_shell", "webtools", "workspace"},
		"the toolsets that shipped today must all be named")
	require.NotContains(t, detected[0].Toolsets, "local_fs")
	require.NotContains(t, detected[0].Toolsets, "git")

	notice := stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil)
	require.Contains(t, notice, detected[0].Path, "the notice names the file by full path — a shadowing copy must be findable")
	require.Contains(t, notice, "workspace")
	require.Contains(t, notice, "default_action")
	require.Contains(t, notice, "stops for approval")
	require.Contains(t, notice, "stay visible to the model",
		"the notice must not read as if the rule list gates tool availability")
	require.Contains(t, notice, RefreshPoliciesCommand)
	require.NotContains(t, notice, "\n", "the startup notice is one line, not a wall")
}

// TestUnit_StalePolicyNotice_StopsAfterRefresh asserts the notice appears until the envelope gains the rules, by refresh or by hand, and never again after.
func TestUnit_StalePolicyNotice_StopsAfterRefresh(t *testing.T) {
	t.Parallel()
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`

	t.Run("the refresh verb silences it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))
		require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil))

		// What `contenox init --refresh-policies` does.
		require.NoError(t, writeEmbeddedHITLPolicies(dir, true))

		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil))
		require.Empty(t, stalePolicyPresets([]string{dir}, nil))
		// And it stays silent on the next run, without rewriting anything.
		stale, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stale)
		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil))
	})

	t.Run("the operator adding the rules by hand silences it too", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))
		require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil))

		// Their own envelope, their own verdicts — some allow, some deny.
		var b strings.Builder
		b.WriteString(`{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}`)
		for _, toolset := range stalePolicyPresets([]string{dir}, nil)[0].Toolsets {
			b.WriteString(`,{"tools":"` + toolset + `","action":"deny"}`)
		}
		b.WriteString(`]}`)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(b.String()), 0o644))

		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil),
			"the notice must stop once the envelope rules on the toolsets, whatever it decides")
	})

	t.Run("a fresh install never sees it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := upgradeEmbeddedHITLPolicies(dir, false)
		require.NoError(t, err)
		require.Empty(t, stalePolicyPresets([]string{dir}, nil))
		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil))
	})

	t.Run("a preset that is not on disk is not stale", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.Empty(t, stalePolicyPresets([]string{dir}, nil), "an empty dir gets this build's presets written to it")
		require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, nil))
		require.Empty(t, stalePolicyNotice("not-a-shipped-preset.json", []string{dir}, nil))
	})
}

// TestUnit_StalePolicyPresets_ReadsTheFileTheLoaderWouldRead asserts staleness is judged on the first policy file along the search path, matching the loader.
func TestUnit_StalePolicyPresets_ReadsTheFileTheLoaderWouldRead(t *testing.T) {
	t.Parallel()
	workspace, home := t.TempDir(), t.TempDir()
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(home, "hitl-policy-default.json"), []byte(previous), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "hitl-policy-default.json"),
		[]byte(shippedPreset(t, "hitl-policy-default.json")), 0o644))

	require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{workspace, home}, nil),
		"the workspace copy wins, and it is current")
	require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{home, workspace}, nil),
		"with the stale copy first, that is the one the loader reads")
}

// TestUnit_RunRefreshPolicies_RewritesShadowingWorkspaceCopy asserts the refresh verb rewrites the copy the loader actually reads — a stale workspace preset shadowing ~/.contenox — and records provenance where it wrote.
func TestUnit_RunRefreshPolicies_RewritesShadowingWorkspaceCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".contenox")
	workspace := t.TempDir()

	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.MkdirAll(globalDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "hitl-policy-default.json"), []byte(previous), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "hitl-policy-default.json"), []byte(previous), 0o644))

	var out strings.Builder
	require.NoError(t, runRefreshPolicies(&out, workspace, nil))

	onDisk, err := os.ReadFile(filepath.Join(workspace, "hitl-policy-default.json"))
	require.NoError(t, err)
	require.Equal(t, shippedPreset(t, "hitl-policy-default.json"), string(onDisk),
		"the workspace copy is the one the loader reads, so the refresh must rewrite it")
	require.Empty(t, stalePolicyPresets(policyDirs(workspace), nil))

	require.Contains(t, out.String(), filepath.Join(workspace, "hitl-policy-default.json"))
	require.Contains(t, out.String(), filepath.Join(globalDir, "hitl-policy-default.json"))
	require.Contains(t, out.String(), "HITL policy presets are now this build's")
	require.NotContains(t, out.String(), "Still stale")

	// Presets the workspace never held are not seeded into it — that would
	// widen the shadow; they keep resolving to the refreshed home copies.
	require.NoFileExists(t, filepath.Join(workspace, "hitl-policy-strict.json"))
	require.FileExists(t, filepath.Join(globalDir, "hitl-policy-strict.json"))

	// Provenance lands beside the rewritten copy, so a future upgrade can
	// prove it untouched.
	require.Equal(t, presetSHA(shippedPreset(t, "hitl-policy-default.json")),
		readPresetState(workspace)["hitl-policy-default.json"])
}

// TestUnit_RunRefreshPolicies_ReportsResidualStaleness asserts the closing line is earned by the detector: a shadowing copy the refresh could not rewrite is named with its full path and fall-through, never papered over with success.
func TestUnit_RunRefreshPolicies_ReportsResidualStaleness(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only file does not stop root from rewriting it")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()

	previous := `{"default_action":"deny","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	stalePath := filepath.Join(workspace, "hitl-policy-default.json")
	require.NoError(t, os.WriteFile(stalePath, []byte(previous), 0o444))

	var out strings.Builder
	require.NoError(t, runRefreshPolicies(&out, workspace, nil))

	require.Contains(t, out.String(), "Still stale: "+stalePath)
	require.Contains(t, out.String(), "every call is denied",
		"the residual report states that file's own default_action fall-through")
	require.NotContains(t, out.String(), "HITL policy presets are now this build's",
		"a refresh that left the loader's copy stale must not claim success")
}

// TestUnit_DoctorStalePolicyWarning asserts doctor renders the stale-preset warning without ever reporting a blocked runtime.
func TestUnit_DoctorStalePolicyWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(previous), 0o644))

	res := setupcheck.Result{DefaultModel: "qwen2.5:7b", DefaultProvider: "ollama"}
	res = setupcheck.AddStalePolicyPresetIssue(res, stalePolicyPresetIssues([]string{dir}, nil), RefreshPoliciesCommand)

	var out strings.Builder
	printDoctorText(&out, res)
	text := out.String()
	require.Contains(t, text, "[warning]")
	require.Contains(t, text, filepath.Join(dir, "hitl-policy-default.json"),
		"the warning names the file by full path — a shadowing copy must be findable")
	require.Contains(t, text, "workspace")
	require.Contains(t, text, "default_action")
	require.Contains(t, text, "stay visible to the model",
		"the warning must not read as if the rule list gates tool availability")
	require.Contains(t, text, RefreshPoliciesCommand)
	require.NotContains(t, text, "All checks passed")

	require.Empty(t, res.BlockingIssues(), "a stale envelope must never block")
	require.True(t, res.Ready())

	// And with a current envelope doctor stays quiet.
	require.NoError(t, writeEmbeddedHITLPolicies(dir, true))
	clean := setupcheck.AddStalePolicyPresetIssue(
		setupcheck.Result{DefaultModel: "qwen2.5:7b", DefaultProvider: "ollama"},
		stalePolicyPresetIssues([]string{dir}, nil), RefreshPoliciesCommand)
	var cleanOut strings.Builder
	printDoctorText(&cleanOut, clean)
	require.Contains(t, cleanOut.String(), "All checks passed.")
}

// TestUnit_StaleDetection_SkipsBetaGatedToolsets asserts a missing rule for a
// toolset gated behind opt-in-beta (goja, shell_session) is not staleness when
// the gate is off — the toolset does not exist for that user — and is again
// when the gate is on.
func TestUnit_StaleDetection_SkipsBetaGatedToolsets(t *testing.T) {
	t.Parallel()
	shipped := `{
		"default_action": "approve",
		"rules": [
			{"tools": "local_fs", "tool": "read_file", "action": "allow"},
			{"tools": "goja", "tool": "goja_eval", "action": "allow"},
			{"tools": "shell_session", "tool": "shell_session_read", "action": "allow"}
		]
	}`
	onDisk := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`

	off := betaGatedToolsets(false)
	require.Equal(t, map[string]bool{"goja": true, "shell_session": true}, off,
		"the gated set names exactly the beta toolsets")
	require.Empty(t, missingPolicyToolsets([]byte(shipped), []byte(onDisk), off),
		"invisible toolsets must not be reported stale")

	on := betaGatedToolsets(true)
	require.Nil(t, on)
	require.Equal(t, []string{"goja", "shell_session"},
		missingPolicyToolsets([]byte(shipped), []byte(onDisk), on),
		"with the gate on the same file is stale for both")

	// The shipped presets rule on both gated toolsets, so a file ruling on
	// everything else the shipped default rules on is stale only for a user
	// who opted in.
	want, ok := policyToolsets([]byte(shippedPreset(t, "hitl-policy-default.json")))
	require.True(t, ok)
	require.True(t, want["goja"] && want["shell_session"],
		"the shipped default preset must rule on both gated toolsets")
	var b strings.Builder
	b.WriteString(`{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}`)
	for name := range want {
		if name == "goja" || name == "shell_session" || name == catchAllToolset {
			continue
		}
		b.WriteString(`,{"tools":"` + name + `","action":"deny"}`)
	}
	b.WriteString(`]}`)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hitl-policy-default.json"), []byte(b.String()), 0o644))
	require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, off),
		"an envelope missing only gated toolsets is current for a stable user")
	require.NotEmpty(t, stalePolicyNotice("hitl-policy-default.json", []string{dir}, on),
		"the same envelope is stale once the operator opts in")
}

// TestUnit_PolicyDirs asserts policyDirs mirrors hitlPolicySource's search path,
// dedupes when the primary dir is already $HOME/.contenox, and orders the
// envelopes rendered from agent declarations last so a hand-written envelope of
// the same name shadows a rendered one.
func TestUnit_PolicyDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".contenox")
	generated := func(dir string) string { return filepath.Join(dir, agentdecl.GeneratedDirName) }

	require.Equal(t,
		[]string{"/work/.contenox", globalDir, generated("/work/.contenox"), generated(globalDir)},
		policyDirs("/work/.contenox"))
	require.Equal(t,
		[]string{globalDir, generated(globalDir)},
		policyDirs(globalDir), "beam resolves both to ~/.contenox")
	require.Equal(t,
		[]string{globalDir, generated(globalDir)},
		policyDirs(""), "an unnamed primary dir contributes nothing, least of all a relative .generated")
}
