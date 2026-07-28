package localtools_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// TestUnit_Grep_DirectorySearchesRecursively asserts a directory path searches
// every file beneath it, formatting hits as "relpath:N: text".
func TestUnit_Grep_DirectorySearchesRecursively(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc Needle() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("package sub\n\nfunc Other() {}\n"), 0o644))

	h := localtools.NewLocalFSTools(root, nil)
	res, dt, err := h.Exec(context.Background(), time.Now(), map[string]any{
		"path": ".", "pattern": "Needle",
	}, false, &taskengine.ToolsCall{ToolName: "grep"})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeString, dt)
	out := res.(string)
	require.Contains(t, out, "a.go:3: func Needle() {}")
	require.NotContains(t, out, "sub/b.go")
}

// TestUnit_Grep_DirectorySkipsBinariesAndNoiseDirs asserts binary files and
// default high-noise directories (e.g. node_modules) never surface as matches.
func TestUnit_Grep_DirectorySkipsBinariesAndNoiseDirs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "node_modules", "lib.js"), []byte("needle\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "blob.bin"), append([]byte("needle\x00"), make([]byte, 32)...), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("needle here\n"), 0o644))

	h := localtools.NewLocalFSTools(root, nil)
	res, _, err := h.Exec(context.Background(), time.Now(), map[string]any{
		"path": ".", "pattern": "needle",
	}, false, &taskengine.ToolsCall{ToolName: "grep"})
	require.NoError(t, err)
	out := res.(string)
	require.Contains(t, out, "keep.txt")
	require.NotContains(t, out, "node_modules")
	require.NotContains(t, out, "blob.bin")
}

// TestUnit_Grep_DirectoryRegexMode asserts regex=true works across a directory search too.
func TestUnit_Grep_DirectoryRegexMode(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("line one\nline two\nline three\n"), 0o644))

	h := localtools.NewLocalFSTools(root, nil)
	res, _, err := h.Exec(context.Background(), time.Now(), map[string]any{
		"path": ".", "pattern": `^line \w+$`, "regex": true,
	}, false, &taskengine.ToolsCall{ToolName: "grep"})
	require.NoError(t, err)
	out := res.(string)
	require.Contains(t, out, "a.txt:1: line one")
	require.Contains(t, out, "a.txt:2: line two")
	require.Contains(t, out, "a.txt:3: line three")
}

// TestUnit_Grep_DirectoryHardCapsAtOneHundredMatchesRegardlessOfPolicy asserts
// the directory-search match cap never exceeds dirGrepMaxMatches even when
// _max_grep_matches is set far higher, and the truncation notice names the cap.
func TestUnit_Grep_DirectoryHardCapsAtOneHundredMatchesRegardlessOfPolicy(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 150; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d.txt", i)), []byte("needle\n"), 0o644))
	}

	h := localtools.NewLocalFSTools(root, nil)
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{
		"_max_grep_matches": "100000",
	})
	res, _, err := h.Exec(ctx, time.Now(), map[string]any{
		"path": ".", "pattern": "needle",
	}, false, &taskengine.ToolsCall{ToolName: "grep"})
	require.NoError(t, err)
	out := res.(string)
	require.Equal(t, 100, strings.Count(out, ": needle"), "directory grep must never return more than the hard cap regardless of policy")
	require.Contains(t, out, "truncated")
	require.Contains(t, out, "100-match")
	require.Contains(t, out, "(recoverable:")
}
