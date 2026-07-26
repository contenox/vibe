package libsandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_buildPlan_CanonicalizesSymlinkedTopLevel is the FIX 6b regression: a
// top-level path that is a SYMLINK — the workspace or a carve-out — is recorded in
// the plan as its RESOLVED target, so the Landlock rule is anchored to the real
// inode (pinned here in the trusted parent) rather than followed later in the shim,
// where the named link could grant an unintended target.
func TestUnit_buildPlan_CanonicalizesSymlinkedTopLevel(t *testing.T) {
	base := t.TempDir()

	// A real carve-out target and a symlink pointing at it.
	target := filepath.Join(base, "realcfg")
	require.NoError(t, os.MkdirAll(target, 0o755))
	link := filepath.Join(base, "linkcfg")
	require.NoError(t, os.Symlink(target, link))

	// A real workspace reached through a symlink too.
	realWS := filepath.Join(base, "realws")
	require.NoError(t, os.MkdirAll(realWS, 0o755))
	wsLink := filepath.Join(base, "linkws")
	require.NoError(t, os.Symlink(realWS, wsLink))

	plan, err := buildPlan(Spec{
		WorkspaceRoot: wsLink,
		Home:          t.TempDir(),
		FS:            []FSCarveout{{Path: link, Mode: ModeRO, Needs: "x"}},
	}, "/bin/true", []string{"true"})
	require.NoError(t, err)

	wantTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	wantWS, err := filepath.EvalSymlinks(realWS)
	require.NoError(t, err)

	require.Len(t, plan.FS, 1)
	require.Equal(t, wantTarget, plan.FS[0].Path,
		"a symlinked carve-out must be pinned to its resolved target, not the link")
	require.Equal(t, wantWS, plan.Workspace,
		"a symlinked workspace must be pinned to its resolved target")
}

// A carve-out that does not exist has no symlink to pin, so canonicalization falls
// back to a lexical clean and the (absolute) path is carried through unchanged for
// the shim to skip as missing.
func TestUnit_buildPlan_MissingCarveoutFallsBackToClean(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	plan, err := buildPlan(Spec{
		WorkspaceRoot: t.TempDir(),
		Home:          t.TempDir(),
		FS:            []FSCarveout{{Path: missing, Mode: ModeRO, Needs: "x"}},
	}, "/bin/true", []string{"true"})
	require.NoError(t, err)
	require.Len(t, plan.FS, 1)
	require.Equal(t, filepath.Clean(missing), plan.FS[0].Path)
}
