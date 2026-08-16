package contenoxcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// What `contenox init` leaves in view is the product's first sentence about
// itself. A directory of chain JSON said "this is a chain engine" before the
// operator read a word.
func TestUnit_GlobalInit_LeavesNoChainJSONAtTheTopLevel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	require.NoError(t, RunGlobalInit(&out))

	contenoxDir := filepath.Join(home, ".contenox")
	entries, err := os.ReadDir(contenoxDir)
	require.NoError(t, err)

	var chainFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "chain-") {
			chainFiles = append(chainFiles, e.Name())
		}
	}
	require.Empty(t, chainFiles, "no chain file belongs at the top level of ~/.contenox")

	for _, f := range initChainFiles {
		require.FileExists(t, filepath.Join(contenoxDir, SystemDirName, f.Name))
	}
	// The envelopes stay in view: they are what the agent runs under, and the
	// whole point is that a human can read and argue with them.
	require.FileExists(t, filepath.Join(contenoxDir, "hitl-policy-default.json"))
	require.FileExists(t, filepath.Join(contenoxDir, "agents.toml"))
}

// The regression that would be silent: the shipped agents are discovered from
// chain files on disk, so moving those files must not empty the roster.
func TestUnit_ShippedAgentsStillRegisterFromSystemDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	require.NoError(t, RunGlobalInit(&out))
	contenoxDir := filepath.Join(home, ".contenox")

	dbPath := filepath.Join(t.TempDir(), "agents.db")
	ctx, svc, done := openServiceAt(t, dbPath)
	defer done()

	discoverChainAgents(ctx, svc, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{})

	for _, name := range []string{"chain-acp", "chain-acpx"} {
		_, err := svc.GetByName(ctx, name)
		require.NoErrorf(t, err, "shipped agent %q must still register from %s/", name, SystemDirName)
	}
}

func TestUnit_LookupSystemFile_PrefersAnOperatorCopyOverTheShippedOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	require.NoError(t, RunGlobalInit(&out))
	contenoxDir := filepath.Join(home, ".contenox")

	shipped, err := lookupSystemFile("", chainPlannerDefaultFilename)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(contenoxDir, SystemDirName, chainPlannerDefaultFilename), shipped)

	// Copying one up a level is how you take ownership of it.
	owned := filepath.Join(contenoxDir, chainPlannerDefaultFilename)
	require.NoError(t, os.WriteFile(owned, []byte(`{"id":"chain-planner","tasks":[]}`), 0o644))

	got, err := lookupSystemFile("", chainPlannerDefaultFilename)
	require.NoError(t, err)
	require.Equal(t, owned, got, "an operator copy at the top level wins")
}

func TestUnit_MigrateChainsIntoSystemDir_MovesOnlyUnmodifiedFiles(t *testing.T) {
	dir := t.TempDir()

	untouched := initChainFiles[0]
	customised := initChainFiles[1]
	require.NoError(t, os.WriteFile(filepath.Join(dir, untouched.Name), []byte(untouched.Content), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, customised.Name), []byte(`{"id":"mine","tasks":[]}`), 0o644))

	var out bytes.Buffer
	require.NoError(t, migrateChainsIntoSystemDir(&out, dir))

	require.NoFileExists(t, filepath.Join(dir, untouched.Name), "an unmodified shipped file is relocated")
	require.FileExists(t, filepath.Join(dir, SystemDirName, untouched.Name))

	require.FileExists(t, filepath.Join(dir, customised.Name), "a customised file is the operator's and stays put")
	body, err := os.ReadFile(filepath.Join(dir, customised.Name))
	require.NoError(t, err)
	require.Equal(t, `{"id":"mine","tasks":[]}`, string(body))
}

// An operator who customised a chain must not silently get the shipped one
// written underneath, nor lose theirs on the next init.
func TestUnit_GlobalInit_LeavesACustomisedChainOwningItsName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	contenoxDir := filepath.Join(home, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))

	mine := filepath.Join(contenoxDir, chainPlannerDefaultFilename)
	require.NoError(t, os.WriteFile(mine, []byte(`{"id":"chain-planner","tasks":[]}`), 0o644))

	var out bytes.Buffer
	require.NoError(t, RunGlobalInit(&out))

	body, err := os.ReadFile(mine)
	require.NoError(t, err)
	require.Equal(t, `{"id":"chain-planner","tasks":[]}`, string(body), "init must not overwrite a customised chain")
	require.NoFileExists(t, filepath.Join(contenoxDir, SystemDirName, chainPlannerDefaultFilename),
		"no shipped copy is written under a name the operator already owns")

	resolved, err := lookupSystemFile("", chainPlannerDefaultFilename)
	require.NoError(t, err)
	require.Equal(t, mine, resolved)
}
