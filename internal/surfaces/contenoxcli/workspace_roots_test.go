package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/services/workspacegrants"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// workspaceRootCmd is a bare command carrying only the flag under test, so the
// roots source is exercised without booting a surface.
func workspaceRootCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	addWorkspaceRootFlag(cmd)
	require.NoError(t, cmd.Flags().Parse(args))
	return cmd
}

// resolvedRoot is the form the Factory stores: cleaned, absolute, symlink-resolved.
func resolvedRoot(t *testing.T, path string) string {
	t.Helper()
	resolved, err := vfs.ResolveRoot(path)
	require.NoError(t, err)
	return resolved
}

// grantingStore returns a store holding exactly the given durable grants,
// written through the same workspacegrants.Add the `contenox workspace add`
// verb calls, so a test cannot pass against a grant shape the CLI never writes.
func grantingStore(t *testing.T, grants ...string) runtimetypes.Store {
	t.Helper()
	_, store, done := storeAt(t, filepath.Join(t.TempDir(), "grants.db"))
	t.Cleanup(done)
	for _, g := range grants {
		_, err := workspacegrants.Add(t.Context(), store, g)
		require.NoError(t, err)
	}
	return store
}

// denyControlPlane registers dirs as the process-global control plane for the
// duration of one test. The denylist is process-global, so it is always
// cleared again — a leaked deny would silently fail unrelated tests.
func denyControlPlane(t *testing.T, dirs ...string) {
	t.Helper()
	require.NoError(t, vfs.SetControlPlaneDenied(dirs...))
	t.Cleanup(func() { require.NoError(t, vfs.SetControlPlaneDenied()) })
}

// TestUnit_BuildWorkspaceFactory_LaunchDirectoryIsTheDefaultRoot pins the rule
// that fixes the agent-at-"/" failure: whatever else is configured, the launch
// directory is first, and first is what an unspecified cwd resolves to.
func TestUnit_BuildWorkspaceFactory_LaunchDirectoryIsTheDefaultRoot(t *testing.T) {
	launchDir := t.TempDir()

	factory, err := buildWorkspaceFactory(workspaceRootCmd(t), launchDir, nil)
	require.NoError(t, err)
	require.NotNil(t, factory, "a launch directory alone is a configured allowlist")
	require.Equal(t, resolvedRoot(t, launchDir), factory.Default())
	require.Equal(t, []string{resolvedRoot(t, launchDir)}, factory.Roots())

	resolved, err := vfs.ResolveSessionCwd(factory, "/", "")
	require.NoError(t, err)
	require.Equal(t, resolvedRoot(t, launchDir), resolved)
}

// TestUnit_BuildWorkspaceFactory_GrantReachesTheSessionAllowlist is the
// regression test for the defect this file exists to close: `contenox
// workspace` wrote durable grants that nothing read, so an operator could grant
// a root, see it listed, and watch every session refuse it.
func TestUnit_BuildWorkspaceFactory_GrantReachesTheSessionAllowlist(t *testing.T) {
	launchDir := t.TempDir()
	grantedDir := t.TempDir()
	ungrantedDir := t.TempDir()

	factory, err := buildWorkspaceFactory(workspaceRootCmd(t), launchDir, grantingStore(t, grantedDir))
	require.NoError(t, err)

	resolved, err := vfs.ResolveSessionCwd(factory, grantedDir, "")
	require.NoError(t, err, "a granted root must be a permitted session cwd")
	require.Equal(t, resolvedRoot(t, grantedDir), resolved)

	sub := filepath.Join(grantedDir, "pkg", "inner")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	resolvedSub, err := vfs.ResolveSessionCwd(factory, sub, "")
	require.NoError(t, err, "granting a root grants everything under it")
	require.Equal(t, resolvedRoot(t, sub), resolvedSub)

	_, err = vfs.ResolveSessionCwd(factory, ungrantedDir, "")
	require.ErrorIs(t, err, vfs.ErrCwdNotPermitted,
		"a directory nobody granted stays outside the allowlist")
}

// TestUnit_BuildWorkspaceFactory_GrantsAndFlagsUnion pins the precedence rule
// stated in workspace_roots.go: the sources union, nothing overrides anything,
// and the launch directory keeps position zero — so a flag can neither displace
// the default root nor shadow a durable grant.
func TestUnit_BuildWorkspaceFactory_GrantsAndFlagsUnion(t *testing.T) {
	launchDir := t.TempDir()
	grantedDir := t.TempDir()
	flagDir := t.TempDir()
	envDir := t.TempDir()

	t.Setenv(workspaceRootsEnv, envDir)
	cmd := workspaceRootCmd(t, "--workspace-root", flagDir)
	factory, err := buildWorkspaceFactory(cmd, launchDir, grantingStore(t, grantedDir))
	require.NoError(t, err)

	require.Equal(t, resolvedRoot(t, launchDir), factory.Default(),
		"neither a grant nor a flag may take over the default root")
	require.Equal(t, []string{
		resolvedRoot(t, launchDir),
		resolvedRoot(t, grantedDir),
		resolvedRoot(t, flagDir),
		resolvedRoot(t, envDir),
	}, factory.Roots(), "launch directory, then grants, then flags, then env")

	for _, dir := range []string{grantedDir, flagDir, envDir} {
		_, err := vfs.ResolveSessionCwd(factory, dir, "")
		require.NoErrorf(t, err, "every source contributes to one allowlist: %s", dir)
	}
}

// TestUnit_BuildWorkspaceFactory_DuplicateAcrossSourcesCollapses pins that a
// root reachable from more than one source is one entry, not several: an
// operator who grants a directory and also passes it as a flag has configured
// it once, and a client picking a workspace must not be offered it twice.
func TestUnit_BuildWorkspaceFactory_DuplicateAcrossSourcesCollapses(t *testing.T) {
	launchDir := t.TempDir()
	grantedDir := t.TempDir()

	cmd := workspaceRootCmd(t, "--workspace-root", grantedDir, "--workspace-root", launchDir)
	factory, err := buildWorkspaceFactory(cmd, launchDir, grantingStore(t, grantedDir))
	require.NoError(t, err)

	require.Equal(t, []string{resolvedRoot(t, launchDir), resolvedRoot(t, grantedDir)}, factory.Roots(),
		"the launch directory granted and flagged again is still one root, still first")
}

// TestUnit_BuildWorkspaceFactory_GrantsAreReadWhenTheAllowlistIsBuilt pins the
// documented timing: the grant list is a snapshot taken at build time, so a
// grant written afterward does not appear in an allowlist that already exists.
func TestUnit_BuildWorkspaceFactory_GrantsAreReadWhenTheAllowlistIsBuilt(t *testing.T) {
	launchDir := t.TempDir()
	lateDir := t.TempDir()
	store := grantingStore(t)

	factory, err := buildWorkspaceFactory(workspaceRootCmd(t), launchDir, store)
	require.NoError(t, err)

	_, err = workspacegrants.Add(t.Context(), store, lateDir)
	require.NoError(t, err)

	_, err = vfs.ResolveSessionCwd(factory, lateDir, "")
	require.ErrorIs(t, err, vfs.ErrCwdNotPermitted,
		"a grant written after the allowlist was built applies to the next process, not this one")

	rebuilt, err := buildWorkspaceFactory(workspaceRootCmd(t), launchDir, store)
	require.NoError(t, err)
	_, err = vfs.ResolveSessionCwd(rebuilt, lateDir, "")
	require.NoError(t, err, "the next process picks the grant up")
}

// TestUnit_BuildWorkspaceFactory_ControlPlaneIsRefusedOnEveryPath pins the
// guarantee that has no off switch: no source may put the runtime's own config,
// database, or policies into the allowlist.
func TestUnit_BuildWorkspaceFactory_ControlPlaneIsRefusedOnEveryPath(t *testing.T) {
	controlPlane := t.TempDir()
	denyControlPlane(t, controlPlane)
	launchDir := t.TempDir()

	t.Run("launch directory", func(t *testing.T) {
		_, err := buildWorkspaceFactory(workspaceRootCmd(t), controlPlane, nil)
		require.ErrorIs(t, err, vfs.ErrControlPlane)
	})

	t.Run("subdirectory of the control plane", func(t *testing.T) {
		sub := filepath.Join(controlPlane, "policies")
		require.NoError(t, os.MkdirAll(sub, 0o755))
		_, err := buildWorkspaceFactory(workspaceRootCmd(t), sub, nil)
		require.ErrorIs(t, err, vfs.ErrControlPlane, "the deny covers everything under the control plane")
	})

	t.Run("flag", func(t *testing.T) {
		cmd := workspaceRootCmd(t, "--workspace-root", controlPlane)
		_, err := buildWorkspaceFactory(cmd, launchDir, nil)
		require.ErrorIs(t, err, vfs.ErrControlPlane)
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv(workspaceRootsEnv, controlPlane)
		_, err := buildWorkspaceFactory(workspaceRootCmd(t), launchDir, nil)
		require.ErrorIs(t, err, vfs.ErrControlPlane)
	})

	t.Run("grant is skipped, not fatal", func(t *testing.T) {
		store := grantingStore(t, controlPlane)

		cmd := workspaceRootCmd(t)
		var errOut strings.Builder
		cmd.SetErr(&errOut)

		factory, err := buildWorkspaceFactory(cmd, launchDir, store)
		require.NoError(t, err, "a stale grant must not stop the surface from starting")
		require.Equal(t, []string{resolvedRoot(t, launchDir)}, factory.Roots(),
			"the control-plane grant never reaches the allowlist")
		require.Contains(t, errOut.String(), "control plane")
		require.Contains(t, errOut.String(), "contenox workspace remove",
			"the note names the verb that clears the stale grant")

		_, err = vfs.ResolveSessionCwd(factory, controlPlane, "")
		require.ErrorIs(t, err, vfs.ErrCwdNotPermitted)
	})
}
