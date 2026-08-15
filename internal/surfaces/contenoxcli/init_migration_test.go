package contenoxcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacySeededNames is the old-install fixture set: every basename a pre-v0.38
// init seeded, mirroring legacyChainRenames' keys.
var legacySeededNames = []string{
	"default-chain.json",
	"default-run-chain.json",
	"default-acp-chain.json",
	"headless-acp-chain.json",
	"default-beam-chain.json",
	"default-fim-chain.json",
	"chain-compact.json",
	"agent-planner.json",
}

// writeLegacyInstall writes every legacy seeded basename into dir with content
// distinguishable per file, so a rename (vs a rewrite) is provable.
func writeLegacyInstall(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for _, name := range legacySeededNames {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("LEGACY:"+name), 0o644))
	}
}

// TestUnit_MigrateLegacyChainNames_RenamesByteForByte proves the --update
// rename step moves every shipped legacy basename to its new name with the
// content untouched, and leaves nothing behind under the old names.
func TestUnit_MigrateLegacyChainNames_RenamesByteForByte(t *testing.T) {
	dir := t.TempDir()
	writeLegacyInstall(t, dir)

	var out bytes.Buffer
	require.NoError(t, migrateLegacyChainNames(&out, dir))

	for oldName, newName := range legacyChainRenames {
		_, err := os.Stat(filepath.Join(dir, oldName))
		require.Truef(t, os.IsNotExist(err), "legacy %s must be renamed away", oldName)
		got, err := os.ReadFile(filepath.Join(dir, newName))
		require.NoError(t, err)
		require.Equalf(t, "LEGACY:"+oldName, string(got),
			"%s must carry the legacy file's bytes — rename, never rewrite", newName)
		require.Contains(t, out.String(), "Renamed "+filepath.Join(dir, oldName))
	}
}

// TestUnit_MigrateLegacyChainNames_IsIdempotent proves a second pass finds
// nothing to do and prints nothing.
func TestUnit_MigrateLegacyChainNames_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeLegacyInstall(t, dir)
	var first bytes.Buffer
	require.NoError(t, migrateLegacyChainNames(&first, dir))
	require.Contains(t, first.String(), "Renamed ")

	var second bytes.Buffer
	require.NoError(t, migrateLegacyChainNames(&second, dir))
	require.Empty(t, second.String(), "a second pass must be a silent no-op")
}

// TestUnit_MigrateLegacyChainNames_ConflictKeepsBoth proves that when a legacy
// name and its new name both exist, the new file wins untouched, the legacy
// file stays on disk, and a one-line note is printed — never a clobber.
func TestUnit_MigrateLegacyChainNames_ConflictKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "default-acp-chain.json")
	newPath := filepath.Join(dir, "chain-agent-acp.json")
	require.NoError(t, os.WriteFile(oldPath, []byte("OLD EDIT"), 0o644))
	require.NoError(t, os.WriteFile(newPath, []byte("NEW EDIT"), 0o644))

	var out bytes.Buffer
	require.NoError(t, migrateLegacyChainNames(&out, dir))

	got, err := os.ReadFile(newPath)
	require.NoError(t, err)
	require.Equal(t, "NEW EDIT", string(got), "the new-name file must win untouched")
	got, err = os.ReadFile(oldPath)
	require.NoError(t, err)
	require.Equal(t, "OLD EDIT", string(got), "the legacy file is noted, not removed")
	require.Contains(t, out.String(), "Kept "+newPath)
	require.Contains(t, out.String(), "legacy "+oldPath+" left in place")
}

// TestUnit_RunLocalInitUpdate_MigratesBothTiers proves `init --local --update`
// runs the rename in ~/.contenox AND the workspace .contenox: a workspace
// shadow copy left under its legacy name would silently stop shadowing.
func TestUnit_RunLocalInitUpdate_MigratesBothTiers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	homeContenox := filepath.Join(home, ".contenox")
	writeLegacyInstall(t, homeContenox)

	workspace := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, os.MkdirAll(workspace, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "default-acp-chain.json"), []byte("WS SHADOW"), 0o644))

	var out bytes.Buffer
	require.NoError(t, RunLocalInit(&out, false, true, workspace, ""))

	for oldName, newName := range legacyChainRenames {
		_, err := os.Stat(filepath.Join(homeContenox, oldName))
		require.Truef(t, os.IsNotExist(err), "home %s must be renamed", oldName)
		require.FileExists(t, filepath.Join(homeContenox, newName))
	}
	_, err := os.Stat(filepath.Join(workspace, "default-acp-chain.json"))
	require.True(t, os.IsNotExist(err), "the workspace shadow copy must be renamed too")
	got, err := os.ReadFile(filepath.Join(workspace, "chain-agent-acp.json"))
	require.NoError(t, err)
	require.Equal(t, "WS SHADOW", string(got), "the operator's override keeps its bytes under the new name")

	// Idempotent: the second run renames nothing.
	var again bytes.Buffer
	require.NoError(t, RunLocalInit(&again, false, true, workspace, ""))
	require.NotContains(t, again.String(), "Renamed ")
}

// TestUnit_RunLocalInit_SeedsOnlyConventionNames proves a fresh `init --local`
// writes exactly the chain-<role>-<variant>.json set plus the HITL presets —
// no legacy name is ever seeded again.
func TestUnit_RunLocalInit_SeedsOnlyConventionNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(t.TempDir(), ".contenox")

	var out bytes.Buffer
	require.NoError(t, RunLocalInit(&out, false, false, workspace, ""))

	wantChains := []string{
		"chain-agent-contenox.json",
		"chain-agent-run.json",
		"chain-agent-acp.json",
		"chain-agent-acpx.json",
		"chain-agent-beam.json",
		"chain-fim-default.json",
		"chain-compact-default.json",
		"chain-planner-default.json",
		"chain-oracle-default.json",
	}
	for _, name := range wantChains {
		require.FileExists(t, filepath.Join(workspace, name))
	}
	entries, err := os.ReadDir(workspace)
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		_, isLegacy := legacyChainRenames[name]
		require.Falsef(t, isLegacy, "legacy name %s must never be seeded", name)
		// Only chain files are under test; policies, markers, and the preset
		// provenance file are other machinery's on-disk state.
		if !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "chain-") {
			continue
		}
		require.Containsf(t, wantChains, name, "unexpected seeded chain file %s", name)
	}
}

// TestUnit_InitChainFiles_MatchConventionAndGlobalParity proves the one-name
// rule at the seam: every seeded basename follows chain-<role>-<variant>.json,
// and the doctor shadow set (initSystemFileNames) carries the same names.
func TestUnit_InitChainFiles_MatchConventionAndGlobalParity(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range initChainFiles {
		require.Truef(t, strings.HasPrefix(f.Name, "chain-"), "%s must start with chain-", f.Name)
		require.Truef(t, strings.HasSuffix(f.Name, ".json"), "%s must end with .json", f.Name)
		parts := strings.Split(strings.TrimSuffix(f.Name, ".json"), "-")
		require.GreaterOrEqualf(t, len(parts), 3, "%s must be chain-<role>-<variant>.json", f.Name)
		require.NotEmpty(t, f.Content, "%s must embed non-empty content", f.Name)
		seen[f.Name] = true
	}
	systemNames := initSystemFileNames()
	for _, f := range initChainFiles {
		require.Contains(t, systemNames, f.Name)
	}
	for _, newName := range legacyChainRenames {
		require.Truef(t, seen[newName], "rename target %s must be a seeded file", newName)
	}
}
