package acpsvc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/vfs"
	libacp "github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

// workspaceRootTransport builds a transport whose only interesting dependency
// is the host root, plus a session entry driven natively so the option builders
// are reached through the same dispatch a live session uses.
func workspaceRootTransport(t *testing.T, roots ...string) (*Transport, *sessionEntry) {
	t.Helper()
	deps := Deps{}
	if len(roots) > 0 {
		factory, err := vfs.NewFactory(roots...)
		require.NoError(t, err)
		deps.WorkspaceRoots = factory
	}
	tr := &Transport{deps: deps, defaultProvider: "openai", defaultModel: "gpt-5-mini"}
	return tr, &sessionEntry{driver: &nativeDriver{t: tr}}
}

// hasOption reports whether an option with the given ID was advertised.
func hasOption(options []libacp.SessionConfigOption, id string) bool {
	for _, option := range options {
		if option.ID == id {
			return true
		}
	}
	return false
}

// TestUnit_WorkspaceIsNeverAConfigOption pins the ownership rule: the workspace
// is the client's cwd (editor shape) or the host's one root (serve shape), fixed
// at session/new, and no shape ever advertises a picker — not in the session
// options and not in the initialize _meta snapshot.
func TestUnit_WorkspaceIsNeverAConfigOption(t *testing.T) {
	ctx := context.Background()

	tr, sess := workspaceRootTransport(t)
	require.False(t, hasOption(tr.sessionConfigOptions(ctx, sess), "workspace-root"),
		"the editor shape must not advertise a workspace picker")
	require.False(t, hasOption(tr.workspaceConfigOptions(ctx), "workspace-root"),
		"the initialize _meta snapshot must not either")

	hosted, hostedSess := workspaceRootTransport(t, t.TempDir())
	require.False(t, hasOption(hosted.sessionConfigOptions(ctx, hostedSess), "workspace-root"),
		"a configured host root must not resurrect the picker")
	require.False(t, hasOption(hosted.workspaceConfigOptions(ctx), "workspace-root"),
		"nor in the _meta snapshot")
}

// TestUnit_SessionCwdOutsideHostRootRefused pins the host shape's boundary. A
// client-supplied cwd is untrusted input: with a host root configured, a
// directory outside it is refused rather than adopted, and the unspecified
// forms fall back to the host's root instead of the filesystem root.
func TestUnit_SessionCwdOutsideHostRootRefused(t *testing.T) {
	hostRoot := t.TempDir()
	outside := t.TempDir()
	tr, _ := workspaceRootTransport(t, hostRoot)
	resolvedRoot := tr.deps.WorkspaceRoots.Default()

	_, err := tr.resolveWorkspaceCwd(outside)
	require.Error(t, err, "a cwd outside the host root must be refused")
	require.ErrorContains(t, err, "not under any configured workspace root")

	_, err = tr.resolveWorkspaceCwd(filepath.Join(outside, "nested", "deeper"))
	require.Error(t, err, "a path under a foreign directory must be refused too")

	// The two "unspecified" spellings both mean the host's one root.
	for _, unspecified := range []string{"", "/"} {
		resolved, err := tr.resolveWorkspaceCwd(unspecified)
		require.NoError(t, err, "the %q sentinel must resolve, not error", unspecified)
		require.Equal(t, resolvedRoot, resolved,
			"an unspecified workspace must land in the host root, never at the filesystem root")
	}

	// The root itself, and a subpath of it, are both accepted.
	resolved, err := tr.resolveWorkspaceCwd(hostRoot)
	require.NoError(t, err)
	require.Equal(t, resolvedRoot, resolved)

	// session/new still requires a cwd: absent is a client bug, not a workspace.
	require.Error(t, requireSessionCwd(""))
	require.NoError(t, requireSessionCwd("/"))
}

// TestUnit_SessionCwdClientAuthoritativeWithoutHostRoot pins the editor shape:
// with no host root the client's cwd is adopted as sent, the "/" sentinel names
// nothing and is refused (a session rooted at the filesystem root would contain
// the control plane), and the control plane itself stays refused.
func TestUnit_SessionCwdClientAuthoritativeWithoutHostRoot(t *testing.T) {
	tr, _ := workspaceRootTransport(t)

	project := t.TempDir()
	resolved, err := tr.resolveWorkspaceCwd(project)
	require.NoError(t, err, "the client's cwd is authoritative on the editor path")
	require.Equal(t, project, resolved)

	_, err = tr.resolveWorkspaceCwd("/")
	require.Error(t, err, `"/" names no workspace when no host root is configured`)
	require.ErrorContains(t, err, "names no workspace here")

	controlPlane := t.TempDir()
	require.NoError(t, vfs.SetControlPlaneDenied(controlPlane))
	t.Cleanup(func() { require.NoError(t, vfs.SetControlPlaneDenied()) })
	_, err = tr.resolveWorkspaceCwd(filepath.Join(controlPlane, "nested"))
	require.Error(t, err, "the control plane is never a workspace, host root or not")
}

// TestUnit_SetWorkspaceRootConfigOptionUnknown pins that the retired picker's
// id is a stranger to the config surface: a client that still sends it gets the
// unknown-option refusal, and the session does not move.
func TestUnit_SetWorkspaceRootConfigOptionUnknown(t *testing.T) {
	ctx := context.Background()
	hostRoot := t.TempDir()
	tr, sess := workspaceRootTransport(t, hostRoot)
	resolvedRoot := tr.deps.WorkspaceRoots.Default()
	sess.Cwd = resolvedRoot

	for _, value := range []string{t.TempDir(), resolvedRoot, "/nowhere"} {
		err := tr.setSessionConfigOption(ctx, sess, "workspace-root", value)
		require.Error(t, err, "set_config_option %q on the retired picker must be refused", value)
		require.ErrorContains(t, err, "unknown config option")
	}

	require.Equal(t, resolvedRoot, sess.Cwd, "a refused set must not move the session")
}
