package contenoxcli

import (
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/stretchr/testify/require"
)

// resolvedRoot is the form the Factory stores: cleaned, absolute, symlink-resolved.
func resolvedRoot(t *testing.T, path string) string {
	t.Helper()
	resolved, err := vfs.ResolveRoot(path)
	require.NoError(t, err)
	return resolved
}

// denyControlPlane registers dirs as the process-global control plane for the
// duration of one test. The denylist is process-global, so it is always
// cleared again — a leaked deny would silently fail unrelated tests.
func denyControlPlane(t *testing.T, dirs ...string) {
	t.Helper()
	require.NoError(t, vfs.SetControlPlaneDenied(dirs...))
	t.Cleanup(func() { require.NoError(t, vfs.SetControlPlaneDenied()) })
}

// TestUnit_BuildWorkspaceFactory_OneRootOnly pins the instance=workspace rule:
// the factory holds exactly the root the host was launched to serve — nothing
// widens it — and an unspecified cwd resolves there, never at the filesystem
// root.
func TestUnit_BuildWorkspaceFactory_OneRootOnly(t *testing.T) {
	served := t.TempDir()

	factory, err := buildWorkspaceFactory(served)
	require.NoError(t, err)

	require.Equal(t, []string{resolvedRoot(t, served)}, factory.Roots(),
		"the host serves exactly one workspace")
	require.Equal(t, resolvedRoot(t, served), factory.Default())

	elsewhere := t.TempDir()
	_, ok := factory.Allows(elsewhere)
	require.False(t, ok, "a directory the host was not launched to serve stays out")
}

// TestUnit_BuildWorkspaceFactory_ControlPlaneIsRefused pins that the host
// refuses to serve its own control plane as the workspace, at launch rather
// than at first tool call.
func TestUnit_BuildWorkspaceFactory_ControlPlaneIsRefused(t *testing.T) {
	controlPlane := t.TempDir()
	denyControlPlane(t, controlPlane)

	_, err := buildWorkspaceFactory(filepath.Join(controlPlane, "nested"))
	require.Error(t, err, "a control-plane path must never become the served root")
}
