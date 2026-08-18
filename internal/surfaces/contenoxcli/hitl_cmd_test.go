package contenoxcli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_SpliceTopLevelJSONMember_LeavesEveryOtherByteAlone pins why the
// trust verb splices instead of re-encoding: the shipped presets carry "//"
// annotation keys and a deliberate key order, and an operator's policy file
// must stay diffable against what they wrote.
func TestUnit_SpliceTopLevelJSONMember_LeavesEveryOtherByteAlone(t *testing.T) {
	shipped := []byte(hitlPolicyDefault)
	value := []byte(`{"hashes":{"/usr/bin/git":"abc"}}`)

	inserted, err := spliceTopLevelJSONMember(shipped, "trusted_binaries", value)
	require.NoError(t, err)
	assert.Contains(t, string(inserted), `"//compute": "Generous per-mission ceilings`,
		"annotation keys must survive verbatim")
	assert.Contains(t, string(inserted), string(value))

	// Removing the inserted member's bytes must give back the original file.
	stripped := strings.Replace(string(inserted), "\n  \"trusted_binaries\": "+string(value)+",", "", 1)
	assert.Equal(t, string(shipped), stripped, "insertion must touch nothing else")

	// Replacing an existing member is equally surgical.
	replaced, err := spliceTopLevelJSONMember(inserted, "trusted_binaries", []byte(`{"dirs":["/usr/bin"]}`))
	require.NoError(t, err)
	assert.NotContains(t, string(replaced), `"/usr/bin/git"`, "the old value must be gone")
	assert.Contains(t, string(replaced), `"//compute-fields"`, "and nothing else may move")

	var probe map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(replaced, &probe), "the result must still be valid JSON")
	require.Contains(t, probe, "rules")
	require.Contains(t, probe, "compute")
}

// TestUnit_SpliceTopLevelJSONMember_RejectsWhatItCannotEdit pins that a
// document it cannot read is never rewritten on a guess.
func TestUnit_SpliceTopLevelJSONMember_RejectsWhatItCannotEdit(t *testing.T) {
	for name, doc := range map[string]string{
		"not an object": `["rules"]`,
		"truncated":     `{"rules": [`,
		"not json":      `nonsense`,
	} {
		_, err := spliceTopLevelJSONMember([]byte(doc), "trusted_binaries", []byte(`{}`))
		assert.Errorf(t, err, "%s must not be edited", name)
	}
	// An empty object gets the member with no trailing comma.
	out, err := spliceTopLevelJSONMember([]byte(`{}`), "trusted_binaries", []byte(`{}`))
	require.NoError(t, err)
	var probe map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &probe))
	assert.Contains(t, probe, "trusted_binaries")
}

// TestUnit_WriteTrustedBinaries_RefusesAnInvalidResult pins the guard that
// keeps the verb from writing a policy this build would refuse to load — a
// policy that fails to load falls back to approve-everything, the opposite of
// what declaring a trusted binary is for.
func TestUnit_WriteTrustedBinaries_RefusesAnInvalidResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hitl-policy.json")
	original := []byte(`{"default_action":"approve","rules":[]}`)
	require.NoError(t, os.WriteFile(path, original, 0o644))

	bad := &hitlservice.TrustedBinaries{Hashes: map[string]string{"not/absolute": strings.Repeat("a", 64)}}
	err := writeTrustedBinaries(path, original, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to write")

	onDisk, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, string(original), string(onDisk), "a refused write must leave the file untouched")
}

// TestUnit_HITLTrust_DeclareRefreshRemoveRoundTrip drives the verb's own
// helpers over a real binary, which is the only honest way to test a feature
// whose whole point is that the declaration matches the host.
func TestUnit_HITLTrust_DeclareRefreshRemoveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "mytool")
	if os.PathSeparator == '\\' {
		tool += ".cmd"
	}
	require.NoError(t, os.WriteFile(tool, []byte("#!/bin/sh\necho v1\n"), 0o755))
	real, err := filepath.EvalSymlinks(tool)
	require.NoError(t, err)
	real = filepath.Clean(real)

	tb := &hitlservice.TrustedBinaries{Hashes: map[string]string{}}
	require.NoError(t, declareTrustedEntries(io.Discard, tb, []string{tool}))
	first := tb.Hashes[real]
	require.NotEmpty(t, first, "the declaration must be keyed by the real path")

	// A legitimate upgrade at the same path, then the refresh path.
	require.NoError(t, os.WriteFile(tool, []byte("#!/bin/sh\necho v2\n"), 0o755))
	require.Len(t, hitlservice.CheckTrustedBinaries(tb), 1, "the stale declaration must be reported")
	require.NoError(t, refreshTrustedEntries(io.Discard, tb))
	assert.NotEqual(t, first, tb.Hashes[real], "refresh must re-read the binary")
	assert.Empty(t, hitlservice.CheckTrustedBinaries(tb), "and the declaration must check out again")

	require.NoError(t, removeTrustedEntries(io.Discard, tb, []string{real}))
	assert.Empty(t, tb.Hashes)
}

// TestUnit_CoverDirForBinary_NeverCreatesTheDirsList pins that declaring a
// binary cannot switch the directory check on behind the operator's back —
// one entry in an empty list would refuse everything else.
func TestUnit_CoverDirForBinary_NeverCreatesTheDirsList(t *testing.T) {
	empty := &hitlservice.TrustedBinaries{}
	coverDirForBinary(io.Discard, empty, filepath.Join("/opt", "tools", "mytool"))
	assert.Empty(t, empty.Dirs, "an opted-out directory check must stay off")

	declared := &hitlservice.TrustedBinaries{Dirs: []string{filepath.FromSlash("/usr/bin")}}
	coverDirForBinary(io.Discard, declared, filepath.FromSlash("/opt/tools/mytool"))
	assert.Contains(t, declared.Dirs, filepath.FromSlash("/opt/tools"),
		"an existing list must be extended so the new declaration is reachable")

	before := len(declared.Dirs)
	coverDirForBinary(io.Discard, declared, filepath.FromSlash("/opt/tools/other"))
	assert.Len(t, declared.Dirs, before, "an already-covered directory must not be re-added")
}

// TestUnit_HITLTrust_RefusesToWriteIntoARenderedEnvelope pins the hazard the
// flip introduced: with nothing seeded at the top level, --policy <name>
// resolves to .generated, and a hash written there is discarded by the next
// render without a word.
func TestUnit_HITLTrust_RefusesToWriteIntoARenderedEnvelope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	contenoxDir := filepath.Join(home, ".contenox")
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	require.NoError(t, os.MkdirAll(generated, 0o750))

	name := "hitl-policy-default.json"
	rendered := filepath.Join(generated, name)
	require.NoError(t, os.WriteFile(rendered, []byte(hitlPolicyDefault), 0o644))

	require.True(t, isRenderedPolicyPath(rendered))
	require.False(t, isRenderedPolicyPath(filepath.Join(contenoxDir, name)))

	cmd := newHITLTrustProbeCmd(t, contenoxDir)
	require.NoError(t, cmd.Flags().Set("policy", name))
	err := runHITLTrust(cmd, []string{"go"})
	require.Error(t, err)
	require.Contains(t, err.Error(), rendered)
	require.Contains(t, err.Error(), filepath.Join(contenoxDir, name),
		"the refusal must name the file to copy it to")

	onDisk, readErr := os.ReadFile(rendered)
	require.NoError(t, readErr)
	require.Equal(t, hitlPolicyDefault, string(onDisk), "the rendered envelope must be untouched")

	// --list is a read, so it still reports the rendered envelope.
	listCmd := newHITLTrustProbeCmd(t, contenoxDir)
	require.NoError(t, listCmd.Flags().Set("policy", name))
	require.NoError(t, listCmd.Flags().Set("list", "true"))
	require.NoError(t, runHITLTrust(listCmd, nil))
}

func newHITLTrustProbeCmd(t *testing.T, contenoxDir string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "contenox"}
	root.PersistentFlags().String("data-dir", "", "")
	require.NoError(t, root.PersistentFlags().Set("data-dir", contenoxDir))
	cmd := &cobra.Command{Use: "trust"}
	cmd.Flags().String("policy", "hitl-policy-default.json", "")
	cmd.Flags().Bool("refresh", false, "")
	cmd.Flags().Bool("list", false, "")
	cmd.Flags().Bool("remove", false, "")
	cmd.SetOut(io.Discard)
	root.AddCommand(cmd)
	return cmd
}
