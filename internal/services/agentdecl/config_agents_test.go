package agentdecl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFilename), []byte(body), 0o600))
}

func TestForReturnsRootConfigForAnAgentWithoutASection(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewer.chain]
token_limit = 4096
`)
	cfg, err := Load(dir)
	require.NoError(t, err)

	other, err := cfg.For("triage")
	require.NoError(t, err)
	require.Equal(t, cfg.Chain.TokenLimit, other.Chain.TokenLimit)
}

func TestForAppliesOverlayAndInheritsOmittedKeys(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewer.chain]
token_limit = 4096
`)
	cfg, err := Load(dir)
	require.NoError(t, err)
	require.NotEqual(t, int64(4096), cfg.Chain.TokenLimit, "the root value must differ or the test proves nothing")

	got, err := cfg.For("reviewer")
	require.NoError(t, err)
	require.Equal(t, int64(4096), got.Chain.TokenLimit)
	require.Equal(t, cfg.Chain.MainRounds, got.Chain.MainRounds, "an omitted key keeps the inherited value")
	require.Equal(t, cfg.Chain.MaxTokens, got.Chain.MaxTokens)
	require.Equal(t, cfg.Policy.DefaultAction, got.Policy.DefaultAction)
}

// The reason the overlay is replayed as TOML rather than decoded into a struct
// of pointers: a typed overlay cannot tell an explicit zero from an absent key.
func TestForCarriesExplicitZeroAndFalse(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[chain]
retry_on_failure = 3

[routing]
pin_model = true

[agents.reviewer.chain]
retry_on_failure = 0

[agents.reviewer.routing]
pin_model = false
`)
	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, 3, cfg.Chain.RetryOnFailure)
	require.True(t, cfg.Routing.PinModel)

	got, err := cfg.For("reviewer")
	require.NoError(t, err)
	require.Equal(t, 0, got.Chain.RetryOnFailure, "an explicit 0 must override an inherited 3")
	require.False(t, got.Routing.PinModel, "an explicit false must override an inherited true")
}

func TestForMergesToolsPoliciesPerKnob(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewer.tools_policies.local_shell]
_allowed_commands = "git,go"
`)
	cfg, err := Load(dir)
	require.NoError(t, err)

	got, err := cfg.For("reviewer")
	require.NoError(t, err)
	require.Equal(t, "git,go", got.ToolsPolicies["local_shell"]["_allowed_commands"])
	require.Equal(t, cfg.ToolsPolicies["local_shell"]["_denied_commands"],
		got.ToolsPolicies["local_shell"]["_denied_commands"],
		"an unnamed knob in the same toolset survives")
	require.Equal(t, cfg.ToolsPolicies["local_fs"], got.ToolsPolicies["local_fs"],
		"an unnamed toolset survives")
}

func TestForDoesNotMutateTheRootConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewer.tools_policies.local_shell]
_allowed_commands = "git"

[[agents.reviewer.policy.always_allow]]
tools = "tavily"
tool = "search"
`)
	cfg, err := Load(dir)
	require.NoError(t, err)
	rootShell := cfg.ToolsPolicies["local_shell"]["_allowed_commands"]
	rootAllow := len(cfg.Policy.AlwaysAllow)

	_, err = cfg.For("reviewer")
	require.NoError(t, err)

	require.Equal(t, rootShell, cfg.ToolsPolicies["local_shell"]["_allowed_commands"])
	require.Len(t, cfg.Policy.AlwaysAllow, rootAllow)

	other, err := cfg.For("triage")
	require.NoError(t, err)
	require.Equal(t, rootShell, other.ToolsPolicies["local_shell"]["_allowed_commands"])
	require.Len(t, other.Policy.AlwaysAllow, rootAllow)
}

// First match wins in the emitted policy, so a per-agent grant that landed
// ahead of the root's credential deny would silently waive it.
func TestForAppendsStandingRulesAfterTheRootsDenies(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[[agents.reviewer.policy.always_allow]]
tools = "tavily"
tool = "search"

[[agents.reviewer.policy.always_deny]]
tools = "local_shell"
tool = "local_shell"
`)
	cfg, err := Load(dir)
	require.NoError(t, err)
	rootDenies := len(cfg.Policy.AlwaysDeny)
	require.NotZero(t, rootDenies, "the shipped credential deny must exist")

	got, err := cfg.For("reviewer")
	require.NoError(t, err)
	require.Len(t, got.Policy.AlwaysDeny, rootDenies+1)
	for i, want := range cfg.Policy.AlwaysDeny {
		require.Equal(t, want, got.Policy.AlwaysDeny[i], "the root's denies keep their position")
	}
	require.Equal(t, "local_shell", got.Policy.AlwaysDeny[rootDenies].Tools)
	require.Equal(t, "tavily", got.Policy.AlwaysAllow[len(got.Policy.AlwaysAllow)-1].Tools)
}

func TestForMergesPosturesPerPosture(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewer.policy.postures.auto_edit]
local_fs_read = "allow"
local_fs_write = "allow"
local_shell = "allow"
`)
	cfg, err := Load(dir)
	require.NoError(t, err)

	got, err := cfg.For("reviewer")
	require.NoError(t, err)
	require.Equal(t, "allow", got.Policy.Postures["auto_edit"].LocalShell)
	require.Equal(t, cfg.Policy.Postures["read_only"], got.Policy.Postures["read_only"],
		"an unnamed posture survives, so Validate still finds the full set")
}

func TestForRejectsAnOverlayThatCannotRun(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewer.chain]
token_limit = 0
`)
	cfg, err := Load(dir)
	require.NoError(t, err)

	_, err = cfg.For("reviewer")
	require.Error(t, err)
	require.Contains(t, err.Error(), "agents.reviewer", "the error names the section to fix")
	require.Contains(t, err.Error(), "token_limit")
}

func TestOverlaysMergePerAgentAcrossRoots(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	writeConfig(t, home, `
[agents.reviewer.chain]
token_limit = 4096
retry_on_failure = 2

[agents.triage.chain]
token_limit = 2048
`)
	writeConfig(t, workspace, `
[agents.reviewer.chain]
token_limit = 8192
`)
	cfg, err := Load(home, workspace)
	require.NoError(t, err)

	reviewer, err := cfg.For("reviewer")
	require.NoError(t, err)
	require.Equal(t, int64(8192), reviewer.Chain.TokenLimit, "the workspace wins")
	require.Equal(t, 2, reviewer.Chain.RetryOnFailure, "a key only home named survives")

	triage, err := cfg.For("triage")
	require.NoError(t, err)
	require.Equal(t, int64(2048), triage.Chain.TokenLimit, "an agent the workspace never named survives")
}

func TestUnknownAgentsReportsMistypedSections(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
[agents.reviewr.chain]
token_limit = 4096

[agents.triage.chain]
token_limit = 2048
`)
	cfg, err := Load(dir)
	require.NoError(t, err)

	require.Equal(t, []string{"reviewr"}, cfg.UnknownAgents(map[string]bool{"triage": true, "reviewer": true}))
	require.Empty(t, cfg.UnknownAgents(map[string]bool{"triage": true, "reviewr": true}))
}
