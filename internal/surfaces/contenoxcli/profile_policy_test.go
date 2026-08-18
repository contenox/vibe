package contenoxcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func policyCmd(t *testing.T, flag string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "probe"}
	registerHITLPolicyFlag(cmd)
	if flag != "" {
		require.NoError(t, cmd.Flags().Set(hitlPolicyFlag, flag))
	}
	return cmd
}

func tempContenoxDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := globalContenoxDir()
	require.NoError(t, err)
	return dir
}

// TestUnit_ProfilePolicy_SelfHealsAnEmptyContenoxDir is the boot this seam
// repairs: nothing has rendered an envelope yet, and the surface must still
// come up gated.
func TestUnit_ProfilePolicy_SelfHealsAnEmptyContenoxDir(t *testing.T) {
	dir := tempContenoxDir(t)
	rendered := filepath.Join(dir, agentdecl.GeneratedDirName, "hitl-policy-serve.json")
	require.NoFileExists(t, rendered)

	pol, err := resolveProfilePolicy(context.Background(), policyCmd(t, ""), dir, "serve", libtracker.NoopTracker{})
	require.NoError(t, err)
	require.Equal(t, "hitl-policy-serve.json", pol.Name)
	require.Empty(t, pol.Dir)
	require.FileExists(t, rendered)

	raw, err := pol.source(dir).ReadPolicy(context.Background(), "", pol.Name)
	require.NoError(t, err)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Contains(t, string(doc["//"]), "serve")
}

// TestUnit_ProfilePolicy_OperatorFileShadowsTheRenderedEnvelope is resolution
// step two: a hand-written top-level file wins without editing anything.
func TestUnit_ProfilePolicy_OperatorFileShadowsTheRenderedEnvelope(t *testing.T) {
	dir := tempContenoxDir(t)
	own := filepath.Join(dir, "hitl-policy-serve.json")
	require.NoError(t, os.WriteFile(own, []byte(`{"default_action":"deny","rules":[]}`), 0o644))

	pol, err := resolveProfilePolicy(context.Background(), policyCmd(t, ""), dir, "serve", libtracker.NoopTracker{})
	require.NoError(t, err)
	raw, err := pol.source(dir).ReadPolicy(context.Background(), "", pol.Name)
	require.NoError(t, err)
	require.JSONEq(t, `{"default_action":"deny","rules":[]}`, string(raw))
	// The transpiled copy is still refreshed; it is simply never read.
	require.FileExists(t, filepath.Join(dir, agentdecl.GeneratedDirName, "hitl-policy-serve.json"))
}

// TestUnit_ProfilePolicy_NamedArgumentWins covers resolution step one in both
// forms: a bare envelope name, and a path honoured verbatim.
func TestUnit_ProfilePolicy_NamedArgumentWins(t *testing.T) {
	dir := tempContenoxDir(t)

	byName, err := resolveProfilePolicy(context.Background(), policyCmd(t, "acpx"), dir, "serve", libtracker.NoopTracker{})
	require.NoError(t, err)
	require.Equal(t, "hitl-policy-acpx.json", byName.Name)

	byFilename, err := resolveProfilePolicy(context.Background(), policyCmd(t, "hitl-policy-acpx.json"), dir, "serve", libtracker.NoopTracker{})
	require.NoError(t, err)
	require.Equal(t, byName.Name, byFilename.Name, "a name and the filename it renders resolve to one envelope")

	elsewhere := t.TempDir()
	path := filepath.Join(elsewhere, "my-policy.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"default_action":"allow","rules":[]}`), 0o644))
	byPath, err := resolveProfilePolicy(context.Background(), policyCmd(t, path), dir, "serve", libtracker.NoopTracker{})
	require.NoError(t, err)
	require.Equal(t, "my-policy.json", byPath.Name)
	raw, err := byPath.source(dir).ReadPolicy(context.Background(), "", byPath.Name)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"allow"`)
}

// TestUnit_ProfilePolicy_NamedButMissingIsAHardError: the operator named that
// exact one, so a missing file stays the error they asked for.
func TestUnit_ProfilePolicy_NamedButMissingIsAHardError(t *testing.T) {
	dir := tempContenoxDir(t)

	_, err := resolveProfilePolicy(context.Background(), policyCmd(t, filepath.Join(t.TempDir(), "gone.json")), dir, "serve", libtracker.NoopTracker{})
	require.Error(t, err)

	_, err = resolveProfilePolicy(context.Background(), policyCmd(t, "no-such-envelope"), dir, "serve", libtracker.NoopTracker{})
	require.ErrorContains(t, err, "no envelope")

	_, err = resolveProfilePolicy(context.Background(), policyCmd(t, "Not A Name"), dir, "serve", libtracker.NoopTracker{})
	require.ErrorContains(t, err, "neither an envelope name nor a path")
}

// TestUnit_ProfilePolicy_EveryShippedProfileResolves guards the binding between
// a surface and the envelope table: a profile naming an envelope nobody ships
// would only fail at boot.
func TestUnit_ProfilePolicy_EveryShippedProfileResolves(t *testing.T) {
	dir := tempContenoxDir(t)
	for _, profile := range []acpProfile{acpProfileACP, acpProfileServe, acpProfileBeam, acpProfileACPX} {
		t.Run(profile.name, func(t *testing.T) {
			pol, err := resolveProfilePolicy(context.Background(), policyCmd(t, ""), dir, profile.hitlEnvelope, libtracker.NoopTracker{})
			require.NoError(t, err)
			require.Equal(t, agentdecl.EnvelopePolicyFile(profile.hitlEnvelope), pol.Name)
			require.FileExists(t, filepath.Join(dir, agentdecl.GeneratedDirName, pol.Name))
		})
	}
	require.NoError(t, ensureProfilePolicy(context.Background(), dir, chatProfileEnvelope, false, libtracker.NoopTracker{}))
}

// TestUnit_Vet_ShadowedEnvelopeIsAWarningNamingBoth.
func TestUnit_Vet_ShadowedEnvelopeIsAWarningNamingBoth(t *testing.T) {
	dir := tempContenoxDir(t)
	generated := filepath.Join(dir, agentdecl.GeneratedDirName)
	require.NoError(t, os.MkdirAll(generated, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(generated, "hitl-policy-serve.json"), []byte(`{"default_action":"deny","rules":[]}`), 0o644))
	own := filepath.Join(dir, "hitl-policy-serve.json")
	require.NoError(t, os.WriteFile(own, []byte(`{"default_action":"approve","rules":[]}`), 0o644))

	var out strings.Builder
	require.Equal(t, 0, runVetOnFiles(&out, []string{own}, vetOpts{envelopeSearchPath: policyDirs(dir)}))
	report := out.String()
	require.Contains(t, report, "WARN "+own)
	require.Contains(t, report, filepath.Join(generated, "hitl-policy-serve.json"))
}

// TestUnit_Vet_RuleNamingAToolTheToolsetDoesNotServeFails.
func TestUnit_Vet_RuleNamingAToolTheToolsetDoesNotServeFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hitl-policy-typo.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
		"default_action": "approve",
		"rules": [
			{"tools": "local_fs", "tool": "raed_file", "action": "allow"},
			{"tools": "tavily", "tool": "search", "action": "allow"}
		]
	}`), 0o644))

	var out strings.Builder
	require.Equal(t, 1, runVetOnFiles(&out, []string{path}, vetOpts{}))
	report := out.String()
	require.Contains(t, report, `serves no tool "raed_file"`)
	// A connected toolset is not this build's to enumerate, so it is left alone.
	require.NotContains(t, report, "tavily")
}
