package localfileservice_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/services/localfileservice"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/stretchr/testify/require"
)

// collectFind runs Find and returns the emitted entries' paths (in emission order)
// plus the result.
func collectFind(t *testing.T, svc localfileservice.Service, opts localfileservice.FindOptions) ([]string, localfileservice.FindResult) {
	t.Helper()
	var paths []string
	res, err := svc.Find(context.Background(), opts, func(e localfileservice.Entry) error {
		paths = append(paths, e.Path)
		return nil
	})
	require.NoError(t, err)
	return paths, res
}

func seedTree(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{"docs", "docs/beam", "src", "node_modules/pkg"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o750))
	}
	for _, f := range []string{
		"README.md",
		"docs/intro.md",
		"docs/beam/guide.md",
		"docs/beam/guide.txt",
		"src/app.ts",
		"src/app_test.ts",
		"node_modules/pkg/index.md", // under a noise dir — must be pruned
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644))
	}
}

func TestUnit_Find_GlobRecursesAndMatchesByExtension(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root)
	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	paths, res := collectFind(t, svc, localfileservice.FindOptions{
		Globs:    []string{"*.md"},
		SkipDirs: map[string]bool{"node_modules": true},
	})

	require.ElementsMatch(t, []string{
		"README.md", "docs/intro.md", "docs/beam/guide.md",
	}, paths, "every .md across the tree, but nothing under the pruned node_modules and no .txt")
	require.False(t, res.Truncated)
	require.Equal(t, 3, res.Count)
}

func TestUnit_Find_MultipleGlobsAreOred(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root)
	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	paths, _ := collectFind(t, svc, localfileservice.FindOptions{
		Globs:    []string{"*.md", "*.ts"},
		SkipDirs: map[string]bool{"node_modules": true},
	})
	require.ElementsMatch(t, []string{
		"README.md", "docs/intro.md", "docs/beam/guide.md", "src/app.ts", "src/app_test.ts",
	}, paths)
}

func TestUnit_Find_PathScopedGlobMatchesRelativePath(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root)
	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	// A pattern with '/' matches the root-relative path, not the basename.
	paths, _ := collectFind(t, svc, localfileservice.FindOptions{Globs: []string{"docs/*.md"}})
	require.ElementsMatch(t, []string{"docs/intro.md"}, paths,
		"docs/*.md matches one level under docs by relative path, not the deeper beam/ file")
}

func TestUnit_Find_SubtreePath(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root)
	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	paths, _ := collectFind(t, svc, localfileservice.FindOptions{Path: "docs/beam", Globs: []string{"*.md"}})
	require.ElementsMatch(t, []string{"docs/beam/guide.md"}, paths,
		"paths stay root-relative even when the walk is scoped to a subtree")
}

func TestUnit_Find_LimitTruncates(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root)
	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	paths, res := collectFind(t, svc, localfileservice.FindOptions{
		Globs:    []string{"*.md"},
		Limit:    2,
		SkipDirs: map[string]bool{"node_modules": true},
	})
	require.Len(t, paths, 2)
	require.True(t, res.Truncated, "hitting the limit sets Truncated")
	require.Equal(t, 2, res.Count)
}

func TestUnit_Find_BadGlobErrors(t *testing.T) {
	svc, err := localfileservice.New(t.TempDir())
	require.NoError(t, err)
	_, ferr := svc.Find(context.Background(), localfileservice.FindOptions{Globs: []string{"["}}, func(localfileservice.Entry) error { return nil })
	require.Error(t, ferr, "a malformed filepath.Match pattern is a request error, not a silent no-match")
}

// TestUnit_Find_SkipsControlPlaneSubtree is the safety-critical case: the walk
// descends into children the single-path guard never saw, so Find MUST re-resolve
// each node through the vfs view. A control-plane directory sitting UNDER a granted
// workspace root must be pruned (never emitted), even though the walk root itself
// is legitimately contained — the recursive analogue of the /files boundary.
func TestUnit_Find_SkipsControlPlaneSubtree(t *testing.T) {
	root := t.TempDir()
	cpDir := filepath.Join(root, "cp")
	lookalike := filepath.Join(root, "cp2") // sibling-named: proves the skip is segment-exact
	require.NoError(t, os.MkdirAll(cpDir, 0o750))
	require.NoError(t, os.MkdirAll(lookalike, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cpDir, "policy.md"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(lookalike, "open.md"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plain.md"), []byte("x"), 0o644))

	require.NoError(t, vfs.SetControlPlaneDenied(cpDir))
	t.Cleanup(func() { require.NoError(t, vfs.SetControlPlaneDenied()) })

	svc, err := localfileservice.New(root)
	require.NoError(t, err)

	paths, _ := collectFind(t, svc, localfileservice.FindOptions{Globs: []string{"*.md"}})
	require.Contains(t, paths, "plain.md", "an ordinary workspace file matches")
	require.Contains(t, paths, "cp2/open.md", "the sibling-named lookalike stays reachable — the skip is segment-exact")
	require.NotContains(t, paths, "cp/policy.md", "a file inside the denied control-plane subtree must never be emitted")
}
