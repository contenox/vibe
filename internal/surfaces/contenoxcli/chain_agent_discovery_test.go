package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// A fresh install has an empty ~/.contenox: only `contenox init` preseeds the
// declarations and only the fleet's discovery pass compiles them, both after the
// chain load. Without this seam no surface could ever boot on a new machine.
func TestUnit_EnsureProfileChain_RecoversAnEmptyContenoxDir(t *testing.T) {
	for _, profile := range []acpProfile{acpProfileACP, acpProfileACPX, acpProfileServe, acpProfileBeam} {
		t.Run(profile.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(profile.chainEnv, "")
			contenoxDir, err := globalContenoxDir()
			require.NoError(t, err)
			require.False(t, acpsvc.ChainFileResolves(contenoxDir, profile.chainFile),
				"a temp home must start without %s", profile.chainFile)
			_, err = acpsvc.LoadChainRegistryFrom(profile.chainFile, profile.chainEnv)
			require.Error(t, err, "the boot this seam repairs must fail without it")

			require.NoError(t, ensureProfileChain(context.Background(), contenoxDir,
				profile.chainFile, profile.chainEnv, libtracker.NoopTracker{}))

			chains, err := acpsvc.LoadChainRegistryFrom(profile.chainFile, profile.chainEnv)
			require.NoError(t, err)
			require.NotEmpty(t, chains.Default().ID)
			require.NotEmpty(t, chains.Default().Tasks)
			require.Equal(t, filepath.Join(contenoxDir, agentdecl.GeneratedDirName, profile.chainFile), chains.Source())
		})
	}
}

// Seeding twice must not fight an operator who edited or deleted a declaration.
func TestUnit_EnsureProfileChain_IsANoOpOnceTheChainResolves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(acpProfileACP.chainEnv, "")
	contenoxDir, err := globalContenoxDir()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, ensureProfileChain(ctx, contenoxDir, acpProfileACP.chainFile, acpProfileACP.chainEnv, libtracker.NoopTracker{}))

	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName, acpProfileACP.chainFile)
	before, err := os.Stat(generated)
	require.NoError(t, err)
	require.NoError(t, ensureProfileChain(ctx, contenoxDir, acpProfileACP.chainFile, acpProfileACP.chainEnv, libtracker.NoopTracker{}))
	after, err := os.Stat(generated)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime(), "a resolvable chain must not be recompiled")
}

// An explicit CONTENOX_ACP_CHAIN_PATH names one exact file: a missing one is the
// operator's error to see, not something to paper over with shipped defaults.
func TestUnit_EnsureProfileChain_LeavesAnExplicitEnvPathAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	missing := filepath.Join(home, "nowhere", "chain-agent-acp.json")
	t.Setenv(acpProfileACP.chainEnv, missing)
	contenoxDir, err := globalContenoxDir()
	require.NoError(t, err)

	require.NoError(t, ensureProfileChain(context.Background(), contenoxDir,
		acpProfileACP.chainFile, acpProfileACP.chainEnv, libtracker.NoopTracker{}))
	require.NoDirExists(t, filepath.Join(contenoxDir, agentdecl.NativeSourceDir),
		"an env override must not trigger seeding")

	_, err = acpsvc.LoadChainRegistryFrom(acpProfileACP.chainFile, acpProfileACP.chainEnv)
	require.ErrorContains(t, err, missing)
}
