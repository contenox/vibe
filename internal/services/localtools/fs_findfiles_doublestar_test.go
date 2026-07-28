package localtools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

func findFilesMatches(t *testing.T, h taskengine.ToolsRepo, args map[string]any) []string {
	t.Helper()
	res, dt, err := h.Exec(context.Background(), time.Now(), args, false, &taskengine.ToolsCall{ToolName: "find_files"})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	var out struct {
		Matches []string `json:"matches"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.(string)), &out))
	return out.Matches
}

// TestUnit_FindFiles_DoubleStarSpansAnyDepth asserts "**" matches zero or more
// intervening directories, including directly in the anchor directory itself.
func TestUnit_FindFiles_DoubleStarSpansAnyDepth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "top.ts"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a", "mid.ts"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a", "b", "deep.ts"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a", "b", "deep.js"), []byte("x"), 0o644))

	h := localtools.NewLocalFSTools(root, nil)
	matches := findFilesMatches(t, h, map[string]any{"pattern": "src/**/*.ts"})
	require.ElementsMatch(t, []string{"src/top.ts", "src/a/mid.ts", "src/a/b/deep.ts"}, matches)
}

// TestUnit_FindFiles_DoubleStarMidPatternStillAnchorsSuffix asserts a "**" not
// at the pattern's end still requires the literal suffix after it to match.
func TestUnit_FindFiles_DoubleStarMidPatternStillAnchorsSuffix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "sub", "internal"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "sub", "internal", "thing_test.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "sub", "internal", "thing.go"), []byte("x"), 0o644))

	h := localtools.NewLocalFSTools(root, nil)
	matches := findFilesMatches(t, h, map[string]any{"pattern": "pkg/**/*_test.go"})
	require.Equal(t, []string{"pkg/sub/internal/thing_test.go"}, matches)
}

// TestUnit_FindFiles_PlainGlobsUnaffectedByDoubleStarSupport is a regression
// guard: patterns without "**" keep their existing basename/relative-path
// matching exactly as before.
func TestUnit_FindFiles_PlainGlobsUnaffectedByDoubleStarSupport(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "subdir", "lib.go"), []byte("x"), 0o644))

	h := localtools.NewLocalFSTools(root, nil)
	matches := findFilesMatches(t, h, map[string]any{"pattern": "*.go"})
	require.ElementsMatch(t, []string{"main.go", "subdir/lib.go"}, matches)

	scoped := findFilesMatches(t, h, map[string]any{"pattern": "subdir/*.go"})
	require.Equal(t, []string{"subdir/lib.go"}, scoped)
}
