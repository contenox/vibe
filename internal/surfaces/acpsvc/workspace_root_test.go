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
// is the allowlist, plus a session entry driven natively so the option builders
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

// TestUnit_WorkspaceRootOptionAbsentWithoutAllowlist pins the "absent, not
// empty" half of the contract: with no allowlist configured the option is not
// advertised at all, so a client hides its workspace picker rather than
// rendering an empty one or erroring on a selection it cannot make.
func TestUnit_WorkspaceRootOptionAbsentWithoutAllowlist(t *testing.T) {
	ctx := context.Background()
	tr, sess := workspaceRootTransport(t)

	_, ok := tr.workspaceRootConfigOption(sess)
	require.False(t, ok, "no allowlist configured must yield no workspace-root option")

	require.False(t, hasOption(tr.sessionConfigOptions(ctx, sess), configIDWorkspaceRoot),
		"session/new must not advertise a workspace picker on the stdio path")
	require.False(t, hasOption(tr.workspaceConfigOptions(ctx), configIDWorkspaceRoot),
		"the initialize _meta snapshot must not advertise one either")
}

// TestUnit_WorkspaceRootOptionAdvertisesAllowlist pins the other half: with an
// allowlist configured the option lists every root and defaults to the launch
// directory, which is the value a client passes back as the new session's cwd.
func TestUnit_WorkspaceRootOptionAdvertisesAllowlist(t *testing.T) {
	ctx := context.Background()
	launchDir := t.TempDir()
	extraDir := t.TempDir()
	tr, sess := workspaceRootTransport(t, launchDir, extraDir)

	resolvedLaunch := tr.deps.WorkspaceRoots.Default()
	resolvedExtra, ok := tr.deps.WorkspaceRoots.Allows(extraDir)
	require.True(t, ok)

	option, ok := tr.workspaceRootConfigOption(sess)
	require.True(t, ok, "a configured allowlist must advertise the option")
	require.Equal(t, configCategoryWorkspaceRoot, option.Category)
	require.Equal(t, configTypeSelect, option.Type)
	require.Equal(t, resolvedLaunch, option.CurrentValue,
		"the launch directory is the default root a session with no chosen workspace gets")

	var values []string
	for _, v := range option.Options.AllValues() {
		values = append(values, v.Value)
	}
	require.ElementsMatch(t, []string{resolvedLaunch, resolvedExtra}, values,
		"the picker must list every allowlisted root")

	require.True(t, hasOption(tr.sessionConfigOptions(ctx, sess), configIDWorkspaceRoot))
	require.True(t, hasOption(tr.workspaceConfigOptions(ctx), configIDWorkspaceRoot),
		"the empty chat reads the allowlist from the initialize _meta snapshot")

	// A session that already chose a root reports that root, not the default.
	sess.Cwd = resolvedExtra
	chosen, ok := tr.workspaceRootConfigOption(sess)
	require.True(t, ok)
	require.Equal(t, resolvedExtra, chosen.CurrentValue)
}

// TestUnit_SessionCwdOutsideAllowlistRefused pins the security boundary. A
// client-supplied cwd is untrusted input: with an allowlist configured, a
// directory outside it is refused rather than adopted, and the unspecified
// forms fall back to the launch directory instead of the filesystem root.
func TestUnit_SessionCwdOutsideAllowlistRefused(t *testing.T) {
	launchDir := t.TempDir()
	outside := t.TempDir()
	tr, _ := workspaceRootTransport(t, launchDir)
	resolvedLaunch := tr.deps.WorkspaceRoots.Default()

	_, err := tr.resolveWorkspaceCwd(outside)
	require.Error(t, err, "a cwd outside the allowlist must be refused")
	require.ErrorContains(t, err, "not under any configured workspace root")

	_, err = tr.resolveWorkspaceCwd(filepath.Join(outside, "nested", "deeper"))
	require.Error(t, err, "a path under a non-allowlisted directory must be refused too")

	// The two "unspecified" spellings both mean the machine's default root.
	for _, unspecified := range []string{"", "/"} {
		resolved, err := tr.resolveWorkspaceCwd(unspecified)
		require.NoError(t, err, "the %q sentinel must resolve, not error", unspecified)
		require.Equal(t, resolvedLaunch, resolved,
			"an unspecified workspace must land in the launch directory, never at the filesystem root")
	}

	// A root on the allowlist, and a subpath of one, are both accepted.
	resolved, err := tr.resolveWorkspaceCwd(launchDir)
	require.NoError(t, err)
	require.Equal(t, resolvedLaunch, resolved)

	// session/new still requires a cwd: absent is a client bug, not a workspace.
	require.Error(t, requireSessionCwd(""))
	require.NoError(t, requireSessionCwd("/"))
}

// TestUnit_SetWorkspaceRootConfigOptionRefused pins the immutability half of
// the contract: the root is fixed at session/new, so a live change is refused
// rather than silently ignored — including a set to the value the session
// already has, which would otherwise read as support for re-rooting.
func TestUnit_SetWorkspaceRootConfigOptionRefused(t *testing.T) {
	ctx := context.Background()
	launchDir := t.TempDir()
	extraDir := t.TempDir()
	tr, sess := workspaceRootTransport(t, launchDir, extraDir)
	resolvedLaunch := tr.deps.WorkspaceRoots.Default()
	sess.Cwd = resolvedLaunch

	for _, value := range []string{extraDir, resolvedLaunch, "/nowhere"} {
		err := tr.setSessionConfigOption(ctx, sess, configIDWorkspaceRoot, value)
		require.Error(t, err, "set_config_option %q on the workspace root must be refused", value)
		require.ErrorContains(t, err, "cannot be changed after the session starts")
	}

	require.Equal(t, resolvedLaunch, sess.Cwd, "a refused set must not move the session")
}
