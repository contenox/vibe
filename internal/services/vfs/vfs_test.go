package vfs_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/services/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_Contain_AllowsPathsWithinRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("x"), 0o644))

	// Relative candidate.
	got, err := vfs.Contain(root, "sub/a.txt")
	require.NoError(t, err)
	real, _ := filepath.EvalSymlinks(filepath.Join(root, "sub", "a.txt"))
	assert.Equal(t, real, got)

	// The root itself.
	got, err = vfs.Contain(root, ".")
	require.NoError(t, err)
	realRoot, _ := filepath.EvalSymlinks(root)
	assert.Equal(t, realRoot, got)

	// A non-existent leaf (write target) validates.
	got, err = vfs.Contain(root, "sub/new.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(realRoot, "sub", "new.txt"), got)
}

func TestUnit_Contain_RejectsTraversalEscape(t *testing.T) {
	root := t.TempDir()
	_, err := vfs.Contain(root, "../escape")
	require.Error(t, err)
	assert.True(t, errors.Is(err, vfs.ErrEscape), "traversal must wrap ErrEscape, got %v", err)
}

func TestUnit_Contain_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	// Reading through an escaping symlink is rejected.
	_, err := vfs.Contain(root, "link/secret.txt")
	require.Error(t, err)
	assert.True(t, errors.Is(err, vfs.ErrEscape))

	// Writing a new file under an escaping symlink is rejected before any I/O.
	_, err = vfs.Contain(root, "link/new.txt")
	require.Error(t, err)
	assert.True(t, errors.Is(err, vfs.ErrEscape))
}

func TestUnit_Within(t *testing.T) {
	root := t.TempDir()
	assert.True(t, vfs.Within(root, filepath.Join(root, "a", "b")))
	assert.True(t, vfs.Within(root, root))
	assert.False(t, vfs.Within(root, filepath.Dir(root)))
	assert.False(t, vfs.Within(root, t.TempDir()))
}

func TestUnit_Factory_AllowlistAndDefault(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	outside := t.TempDir()

	f, err := vfs.NewFactory(a, b, a /* dup ignored */)
	require.NoError(t, err)

	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	assert.Equal(t, []string{ra, rb}, f.Roots())
	assert.Equal(t, ra, f.Default())

	// "/" and "" resolve to the default (compat for clients that send cwd:"/").
	got, ok := f.Allows("/")
	require.True(t, ok)
	assert.Equal(t, ra, got)
	got, ok = f.Allows("")
	require.True(t, ok)
	assert.Equal(t, ra, got)

	// An allowlisted root resolves to itself.
	got, ok = f.Allows(b)
	require.True(t, ok)
	assert.Equal(t, rb, got)

	// A non-allowlisted absolute path is refused.
	_, ok = f.Allows(outside)
	assert.False(t, ok)

	_, err = f.Resolve(outside)
	require.Error(t, err)
	assert.True(t, errors.Is(err, vfs.ErrNotAllowed))
}

func TestUnit_Factory_EmptyIsError(t *testing.T) {
	_, err := vfs.NewFactory()
	require.Error(t, err)
	_, err = vfs.NewFactory("", "")
	require.Error(t, err)
}

// TestUnit_Factory_Containment pins the containment semantics that replaced
// exact-match: a granted root permits itself and
// everything under it, while a sibling, a prefix-trick neighbour, and a symlink
// whose real target escapes every root are all refused. Exact-match used to
// refuse the ok cases too — false protection, since the escape guard is what
// actually bounds a path — and that is the regression this locks out.
func TestUnit_Factory_Containment(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project", "pkg"), 0o755))
	f, err := vfs.NewFactory(root)
	require.NoError(t, err)

	resolvedRoot, err := vfs.ResolveRoot(root)
	require.NoError(t, err)

	t.Run("the root itself is permitted", func(t *testing.T) {
		got, ok := f.Allows(root)
		require.True(t, ok)
		assert.Equal(t, resolvedRoot, got)
	})

	t.Run("a direct subdir is permitted and resolves to the subdir", func(t *testing.T) {
		sub := filepath.Join(root, "project")
		got, ok := f.Allows(sub)
		require.True(t, ok)
		resolvedSub, _ := vfs.ResolveRoot(sub)
		assert.Equal(t, resolvedSub, got)
	})

	t.Run("a deeply nested subdir is permitted", func(t *testing.T) {
		nested := filepath.Join(root, "project", "pkg")
		got, ok := f.Allows(nested)
		require.True(t, ok)
		resolvedNested, _ := vfs.ResolveRoot(nested)
		assert.Equal(t, resolvedNested, got)
	})

	t.Run("a sibling directory is refused", func(t *testing.T) {
		_, ok := f.Allows(t.TempDir())
		assert.False(t, ok)
	})

	t.Run("a prefix-trick neighbour is refused (segments, not string prefix)", func(t *testing.T) {
		// /home/userX must not be read as "under" /home/user. Build the exact
		// shape: a sibling whose name has the root's basename as a string prefix.
		neighbour := resolvedRoot + "X"
		require.NoError(t, os.MkdirAll(neighbour, 0o755))
		t.Cleanup(func() { _ = os.RemoveAll(neighbour) })
		_, ok := f.Allows(neighbour)
		assert.False(t, ok, "%q shares a string prefix with %q but is not under it", neighbour, resolvedRoot)
	})

	t.Run("a symlink under the root whose target escapes every root is refused", func(t *testing.T) {
		outside := t.TempDir()
		link := filepath.Join(root, "escape-link")
		require.NoError(t, os.Symlink(outside, link))
		// The link is lexically under the root, but its real target is not — the
		// symlink-resolving Resolve must refuse it.
		_, ok := f.Allows(link)
		assert.False(t, ok, "a symlink escaping all roots must be refused even though it sits under a root")
	})

	t.Run("a relative cwd is refused before the allowlist is consulted", func(t *testing.T) {
		_, err := vfs.ResolveSessionCwd(f, "relative/path", "")
		require.Error(t, err)
		require.ErrorIs(t, err, vfs.ErrCwdNotPermitted)
		require.Contains(t, err.Error(), "absolute path")
	})
}

// TestUnit_Factory_SetRoots pins the hot-reload writer: SetRoots swaps the whole
// allowlist atomically, readers see the new set immediately, an empty list is
// refused and leaves the old set intact, and roots[0] is the new default.
func TestUnit_Factory_SetRoots(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	f, err := vfs.NewFactory(a)
	require.NoError(t, err)

	ra, _ := vfs.ResolveRoot(a)
	rb, _ := vfs.ResolveRoot(b)

	// Baseline: only a is allowed; b is not.
	_, ok := f.Allows(b)
	require.False(t, ok)

	// Grant b live.
	require.NoError(t, f.SetRoots([]string{a, b}))
	assert.Equal(t, []string{ra, rb}, f.Roots())
	got, ok := f.Allows(b)
	require.True(t, ok)
	assert.Equal(t, rb, got)
	assert.Equal(t, ra, f.Default(), "the first root stays the default across a reload")

	// An empty set is refused and the current allowlist is untouched.
	require.Error(t, f.SetRoots(nil))
	assert.Equal(t, []string{ra, rb}, f.Roots(), "a rejected SetRoots leaves the previous allowlist intact")

	// Revoke b live.
	require.NoError(t, f.SetRoots([]string{a}))
	_, ok = f.Allows(b)
	require.False(t, ok, "a revoked root is refused after reload")
}

func TestUnit_View_ResolveAndContains(t *testing.T) {
	a := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(a, "f.txt"), []byte("x"), 0o644))
	f, err := vfs.NewFactory(a)
	require.NoError(t, err)

	v, err := f.Open("/")
	require.NoError(t, err)
	ra, _ := filepath.EvalSymlinks(a)
	assert.Equal(t, ra, v.Root())

	got, err := v.Resolve("f.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(ra, "f.txt"), got)

	assert.True(t, v.Contains(filepath.Join(ra, "f.txt")))
	assert.False(t, v.Contains(filepath.Dir(ra)))

	_, err = f.Open(t.TempDir())
	require.Error(t, err, "opening a non-allowlisted root must fail")
}

// TestUnit_ResolveSessionCwd pins the ONE decision procedure every session
// bring-up shares — the ACP session/new, session/load and session/resume paths
// and the REST fleet dispatch. Three variants of it used to exist and had
// already drifted: only the ACP entry points guarded against a relative cwd, and
// the two no-allowlist branches disagreed about what an absent cwd means.
func TestUnit_ResolveSessionCwd(t *testing.T) {
	allowed := t.TempDir()
	other := t.TempDir()
	f, err := vfs.NewFactory(allowed)
	require.NoError(t, err)
	resolvedAllowed, err := vfs.ResolveRoot(allowed)
	require.NoError(t, err)

	t.Run("relative cwd is refused with or without an allowlist", func(t *testing.T) {
		for _, factory := range []*vfs.Factory{nil, f} {
			for _, cwd := range []string{"../..", ".", "relative/path"} {
				_, err := vfs.ResolveSessionCwd(factory, cwd, "/fallback")
				require.Error(t, err, "cwd %q", cwd)
				require.ErrorIs(t, err, vfs.ErrCwdNotPermitted)
				require.Contains(t, err.Error(), "absolute path")
			}
		}
	})

	t.Run("no allowlist: an absolute cwd passes through, an absent one takes the caller's fallback", func(t *testing.T) {
		got, err := vfs.ResolveSessionCwd(nil, "/anywhere/at/all", "/fallback")
		require.NoError(t, err)
		assert.Equal(t, "/anywhere/at/all", got, "the editor owns the filesystem on the stdio path")

		// The sentinel is still a path when nothing constrains it.
		got, err = vfs.ResolveSessionCwd(nil, "/", "/fallback")
		require.NoError(t, err)
		assert.Equal(t, "/", got)

		got, err = vfs.ResolveSessionCwd(nil, "", "/fallback")
		require.NoError(t, err)
		assert.Equal(t, "/fallback", got, "unspecified means the CALLER's default root")

		got, err = vfs.ResolveSessionCwd(nil, "", "")
		require.NoError(t, err)
		assert.Equal(t, "", got, "a caller with no default leaves it unspecified")
	})

	t.Run("allowlist: sentinel and empty resolve to the default root", func(t *testing.T) {
		for _, cwd := range []string{"", "/"} {
			got, err := vfs.ResolveSessionCwd(f, cwd, "/ignored")
			require.NoError(t, err, "cwd %q", cwd)
			assert.Equal(t, resolvedAllowed, got,
				"a configured allowlist owns the default; the caller's fallback is not consulted")
		}
	})

	t.Run("allowlist: an allowlisted root resolves, anything else is refused", func(t *testing.T) {
		got, err := vfs.ResolveSessionCwd(f, allowed, "")
		require.NoError(t, err)
		assert.Equal(t, resolvedAllowed, got)

		_, err = vfs.ResolveSessionCwd(f, other, "")
		require.Error(t, err)
		require.ErrorIs(t, err, vfs.ErrCwdNotPermitted)
		require.Contains(t, err.Error(), "not under any configured workspace root",
			"the refusal names the containment rule, not a bare 'not permitted'")
		require.Contains(t, err.Error(), resolvedAllowed,
			"the refusal names the roots the caller may choose from")
	})

	t.Run("allowlist: a directory CONTAINED under a root resolves to the subpath", func(t *testing.T) {
		sub := filepath.Join(allowed, "project", "pkg")
		require.NoError(t, os.MkdirAll(sub, 0o755))
		resolvedSub, err := vfs.ResolveRoot(sub)
		require.NoError(t, err)

		got, err := vfs.ResolveSessionCwd(f, sub, "")
		require.NoError(t, err)
		assert.Equal(t, resolvedSub, got,
			"a contained subdir resolves to the SUBDIR, not the containing root")
	})

	t.Run("the refusal message is the operator-facing text, not the sentinel's", func(t *testing.T) {
		_, err := vfs.ResolveSessionCwd(f, other, "")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), vfs.ErrCwdNotPermitted.Error(),
			"callers forward Error() to a user; the sentinel is for errors.Is, not for reading")
	})
}

// --- Control-plane isolation (vfs-invariant slice, 2026-07-21) ---
//
// The carveout is a process-global denylist (SetControlPlaneDenied), so every
// test here sets it and RESETS it in Cleanup (SetControlPlaneDenied() with no
// args clears it). The vfs_test package runs sequentially (no t.Parallel), so a
// per-test set/reset keeps the global from leaking between cases.

// setControlPlane registers denied and arranges its reset. It returns nothing;
// callers keep using the raw paths.
func setControlPlane(t *testing.T, denied ...string) {
	t.Helper()
	require.NoError(t, vfs.SetControlPlaneDenied(denied...))
	t.Cleanup(func() { _ = vfs.SetControlPlaneDenied() })
}

// TestUnit_ControlPlane_ResolveRefusesRootAndSubpaths pins the root-selection
// half of the carveout: the control-plane dir — a CHILD of a legitimately
// granted root — and every path under it are refused with the distinct
// ErrControlPlane teaching error, while a sibling lookalike (.contenox2) and the
// granted root itself stay permitted.
func TestUnit_ControlPlane_ResolveRefusesRootAndSubpaths(t *testing.T) {
	root := t.TempDir() // the granted workspace root (stands in for /home/user)
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(controlPlane, "local.db"), []byte("db"), 0o600))
	lookalike := filepath.Join(root, ".contenox2")
	require.NoError(t, os.MkdirAll(lookalike, 0o755))

	setControlPlane(t, controlPlane)
	f, err := vfs.NewFactory(root)
	require.NoError(t, err)

	t.Run("the granted root itself is still permitted", func(t *testing.T) {
		_, ok := f.Allows(root)
		assert.True(t, ok, "the carveout must not refuse the legitimate parent root")
	})

	t.Run("the control-plane dir is refused as a root", func(t *testing.T) {
		_, err := f.Resolve(controlPlane)
		require.Error(t, err)
		assert.True(t, errors.Is(err, vfs.ErrControlPlane), "must wrap ErrControlPlane, got %v", err)
		assert.False(t, errors.Is(err, vfs.ErrNotAllowed), "control-plane refusal is DISTINCT from not-under-roots")
		assert.Contains(t, err.Error(), "control plane", "the refusal names the boundary plainly")
	})

	t.Run("a file inside the control plane is refused", func(t *testing.T) {
		_, err := f.Resolve(filepath.Join(controlPlane, "local.db"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, vfs.ErrControlPlane))
	})

	t.Run("a not-yet-created subpath inside the control plane is refused", func(t *testing.T) {
		_, err := f.Resolve(filepath.Join(controlPlane, "models", "llama"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, vfs.ErrControlPlane))
	})

	t.Run("a sibling-named lookalike (.contenox2) is permitted", func(t *testing.T) {
		got, ok := f.Allows(lookalike)
		require.True(t, ok, ".contenox2 shares a string prefix with .contenox but is not under it")
		resolved, _ := vfs.ResolveRoot(lookalike)
		assert.Equal(t, resolved, got)
	})
}

// TestUnit_ControlPlane_SymlinkIntoIsRefused pins that a symlink that sits INSIDE
// a granted root but points INTO the control plane is refused: resolution
// happens first (ResolveRoot follows the link), then the check, so the real
// target is what is judged.
func TestUnit_ControlPlane_SymlinkIntoIsRefused(t *testing.T) {
	root := t.TempDir()
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))
	link := filepath.Join(root, "cplink") // lexically under root, points into the control plane
	require.NoError(t, os.Symlink(controlPlane, link))

	setControlPlane(t, controlPlane)
	f, err := vfs.NewFactory(root)
	require.NoError(t, err)

	_, err = f.Resolve(link)
	require.Error(t, err)
	assert.True(t, errors.Is(err, vfs.ErrControlPlane), "a symlink into the control plane must be refused by its real target, got %v", err)
}

// TestUnit_ControlPlane_ContainmentTraversalRefused pins the traversal half of
// the carveout — the one the /files explorer and the local_fs agent tool hit:
// resolving a RELATIVE path INTO the control plane from a granted parent root
// (vfs.Contain / View.Resolve) is refused, while listing the parent and entering
// a sibling lookalike are not. This is the "show the .contenox entry but refuse
// on enter" behavior in its primitive form.
func TestUnit_ControlPlane_ContainmentTraversalRefused(t *testing.T) {
	root := t.TempDir()
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(controlPlane, "hitl-policy-default.json"), []byte("{}"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".contenox2"), 0o755))

	setControlPlane(t, controlPlane)

	t.Run("Contain refuses entering the control-plane dir relatively", func(t *testing.T) {
		_, err := vfs.Contain(root, ".contenox")
		require.Error(t, err)
		assert.True(t, errors.Is(err, vfs.ErrControlPlane), "got %v", err)
		assert.False(t, errors.Is(err, vfs.ErrEscape), "control-plane refusal is distinct from a root escape")
	})

	t.Run("Contain refuses reading a file inside the control plane relatively", func(t *testing.T) {
		_, err := vfs.Contain(root, ".contenox/hitl-policy-default.json")
		require.Error(t, err)
		assert.True(t, errors.Is(err, vfs.ErrControlPlane))
	})

	t.Run("Contain still resolves the parent root and a sibling lookalike", func(t *testing.T) {
		_, err := vfs.Contain(root, ".")
		require.NoError(t, err, "listing the granted parent is fine — only entering the control plane is refused")
		_, err = vfs.Contain(root, ".contenox2")
		require.NoError(t, err, ".contenox2 is a sibling, not the control plane")
	})

	t.Run("a Factory View over the parent refuses the control plane the same way", func(t *testing.T) {
		f, err := vfs.NewFactory(root)
		require.NoError(t, err)
		v, err := f.Open("/")
		require.NoError(t, err)
		_, err = v.Resolve(".contenox")
		require.Error(t, err)
		assert.True(t, errors.Is(err, vfs.ErrControlPlane))
	})
}

// TestUnit_ControlPlane_SurvivesSetRoots pins that the denylist is NOT part of
// the mutable root set: a SetRoots hot-reload (a grant added or the whole set
// swapped) leaves the control-plane refusal in force.
func TestUnit_ControlPlane_SurvivesSetRoots(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))

	setControlPlane(t, controlPlane)
	f, err := vfs.NewFactory(root)
	require.NoError(t, err)

	_, err = f.Resolve(controlPlane)
	require.ErrorIs(t, err, vfs.ErrControlPlane)

	// Reload the whole allowlist — add `other`, keep `root`.
	require.NoError(t, f.SetRoots([]string{root, other}))
	_, err = f.Resolve(controlPlane)
	require.ErrorIs(t, err, vfs.ErrControlPlane, "the denylist must survive a SetRoots reload")

	// Even swapping to a completely different root set does not clear it.
	require.NoError(t, f.SetRoots([]string{other}))
	_, err = f.Resolve(controlPlane)
	require.ErrorIs(t, err, vfs.ErrControlPlane)
}

// TestUnit_ControlPlane_DeniedRootStillRefused is defense in depth: even if the
// control-plane dir were mistakenly configured AS a workspace root, Resolve still
// refuses it — the denylist is checked before the containment loop, so a bad
// grant cannot re-open the boundary.
func TestUnit_ControlPlane_DeniedRootStillRefused(t *testing.T) {
	controlPlane := t.TempDir() // configured, wrongly, as the only root
	setControlPlane(t, controlPlane)
	f, err := vfs.NewFactory(controlPlane)
	require.NoError(t, err)

	_, err = f.Resolve(controlPlane)
	require.Error(t, err)
	assert.True(t, errors.Is(err, vfs.ErrControlPlane), "a control-plane dir is refused even when it is itself a configured root")
}

// TestUnit_ControlPlane_ResolveSessionCwd pins the session-cwd surface: a cwd at
// or under the control plane is refused with ErrCwdNotPermitted (the transport
// sentinel) but its teaching text names the CONTROL PLANE, distinct from the
// not-under-roots message — with an allowlist AND on the nil (stdio) path.
func TestUnit_ControlPlane_ResolveSessionCwd(t *testing.T) {
	root := t.TempDir()
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))
	setControlPlane(t, controlPlane)
	f, err := vfs.NewFactory(root)
	require.NoError(t, err)

	t.Run("with an allowlist: the control-plane cwd is refused with the distinct teaching text", func(t *testing.T) {
		_, err := vfs.ResolveSessionCwd(f, controlPlane, "")
		require.Error(t, err)
		require.ErrorIs(t, err, vfs.ErrCwdNotPermitted)
		assert.Contains(t, err.Error(), "control plane")
		assert.NotContains(t, err.Error(), "not under any configured workspace root",
			"the control-plane refusal is its own message, not the allowlist message")
	})

	t.Run("on the nil (stdio) path: a registered control plane is still refused", func(t *testing.T) {
		_, err := vfs.ResolveSessionCwd(nil, controlPlane, "")
		require.Error(t, err)
		require.ErrorIs(t, err, vfs.ErrCwdNotPermitted)
		assert.Contains(t, err.Error(), "control plane")
	})
}

// TestUnit_ControlPlane_GrantPredicate pins the pure predicate the grant verbs
// use: WithinControlPlane (explicit denied set, for the CLI) and IsControlPlanePath
// (the process global, for the REST verb). A path at/under a denied dir matches;
// a sibling lookalike does not; an empty denied set matches nothing.
func TestUnit_ControlPlane_GrantPredicate(t *testing.T) {
	root := t.TempDir()
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))
	lookalike := filepath.Join(root, ".contenox2")
	require.NoError(t, os.MkdirAll(lookalike, 0o755))

	t.Run("WithinControlPlane matches the dir and its subpaths with an explicit set", func(t *testing.T) {
		d, ok := vfs.WithinControlPlane([]string{controlPlane}, controlPlane)
		require.True(t, ok)
		assert.Equal(t, controlPlane, d)

		_, ok = vfs.WithinControlPlane([]string{controlPlane}, filepath.Join(controlPlane, "models"))
		assert.True(t, ok)

		_, ok = vfs.WithinControlPlane([]string{controlPlane}, lookalike)
		assert.False(t, ok, ".contenox2 is a sibling, not under .contenox")

		_, ok = vfs.WithinControlPlane(nil, controlPlane)
		assert.False(t, ok, "an empty denied set denies nothing")
	})

	t.Run("IsControlPlanePath consults the process global", func(t *testing.T) {
		setControlPlane(t, controlPlane)
		_, ok := vfs.IsControlPlanePath(filepath.Join(controlPlane, "chains"))
		assert.True(t, ok)
		_, ok = vfs.IsControlPlanePath(lookalike)
		assert.False(t, ok)
	})
}

// TestUnit_ControlPlane_UnregisteredIsNoOp pins that with NO control plane
// registered (the empty global — the stdio/CLI paths and every pre-existing
// test), a .contenox-shaped directory resolves exactly as any other directory:
// the carveout is inert until serve registers a boundary.
func TestUnit_ControlPlane_UnregisteredIsNoOp(t *testing.T) {
	// Deliberately do NOT call setControlPlane.
	root := t.TempDir()
	controlPlane := filepath.Join(root, ".contenox")
	require.NoError(t, os.MkdirAll(controlPlane, 0o700))
	f, err := vfs.NewFactory(root)
	require.NoError(t, err)

	got, ok := f.Allows(controlPlane)
	require.True(t, ok, "with no control plane registered, .contenox is an ordinary subdir")
	resolved, _ := vfs.ResolveRoot(controlPlane)
	assert.Equal(t, resolved, got)

	_, err = vfs.Contain(root, ".contenox")
	require.NoError(t, err, "containment is unaffected when nothing is registered")
}
