package localtools_test

import (
	"context"
	"encoding/json"
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

func browseExec(t *testing.T, h taskengine.ToolsRepo, tool string, args map[string]any) (any, taskengine.DataType) {
	t.Helper()
	res, dt, err := h.Exec(context.Background(), time.Now(), args, false, &taskengine.ToolsCall{ToolName: tool})
	require.NoError(t, err)
	return res, dt
}

func findFilesMatches(t *testing.T, h taskengine.ToolsRepo, args map[string]any) []string {
	t.Helper()
	res, dt := browseExec(t, h, "find_files", args)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	var out struct {
		Matches []string `json:"matches"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.(string)), &out))
	return out.Matches
}

// TestUnit_LocalFSBrowseTools_ListDir asserts a one-level listing marks
// directories and executables and omits high-noise directories.
func TestUnit_LocalFSBrowseTools_ListDir(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, dt := browseExec(t, h, "list_dir", map[string]any{"path": "."})
	require.Equal(t, taskengine.DataTypeString, dt)
	lines := strings.Split(res.(string), "\n")
	require.Equal(t, []string{"a.txt", "run.sh*", "sub/"}, lines)
}

// TestUnit_LocalFSBrowseTools_ListDirRecursiveRespectsMaxDepth asserts
// recursive=true walks only as deep as max_depth.
func TestUnit_LocalFSBrowseTools_ListDirRecursiveRespectsMaxDepth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "one.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "two.txt"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _ := browseExec(t, h, "list_dir", map[string]any{"path": ".", "recursive": true, "max_depth": 2})
	out := res.(string)
	require.Contains(t, out, "a/one.txt")
	require.NotContains(t, out, "a/b/two.txt")

	deep, _ := browseExec(t, h, "list_dir", map[string]any{"path": ".", "recursive": true, "max_depth": 3})
	require.Contains(t, deep.(string), "a/b/two.txt")
}

// TestUnit_LocalFSBrowseTools_ListDirRefusesEscape asserts containment: a path
// outside the allowed directory is refused rather than listed.
func TestUnit_LocalFSBrowseTools_ListDirRefusesEscape(t *testing.T) {
	root := t.TempDir()
	h := localtools.NewLocalFSBrowseTools(root, nil)
	_, _, err := h.Exec(context.Background(), time.Now(), map[string]any{"path": "../.."}, false,
		&taskengine.ToolsCall{ToolName: "list_dir"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes allowed directory")
}

// TestUnit_LocalFSBrowseTools_ListDirRejectsUnknownArgs asserts a misspelled
// argument fails loudly instead of being silently ignored.
func TestUnit_LocalFSBrowseTools_ListDirRejectsUnknownArgs(t *testing.T) {
	h := localtools.NewLocalFSBrowseTools(t.TempDir(), nil)
	_, _, err := h.Exec(context.Background(), time.Now(), map[string]any{"depth": 2}, false,
		&taskengine.ToolsCall{ToolName: "list_dir"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown argument(s): depth")
}

// TestUnit_LocalFSBrowseTools_MissingPathSuggestsSiblings asserts the restored
// "Did you mean:" hint, which the tool descriptions promise.
func TestUnit_LocalFSBrowseTools_MissingPathSuggestsSiblings(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	_, _, err := h.Exec(context.Background(), time.Now(), map[string]any{"path": "config.yml"}, false,
		&taskengine.ToolsCall{ToolName: "stat_file"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
	require.Contains(t, err.Error(), "Did you mean: config.yaml?")
}

// TestUnit_LocalFSBrowseTools_Grep asserts single-file matches print as "N: text"
// within the requested line range.
func TestUnit_LocalFSBrowseTools_Grep(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nbeta\nalpha again\n"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, dt := browseExec(t, h, "grep", map[string]any{"path": "a.txt", "pattern": "alpha"})
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Equal(t, "1: alpha\n3: alpha again", res.(string))

	scoped, _ := browseExec(t, h, "grep", map[string]any{"path": "a.txt", "pattern": "alpha", "start_line": 2})
	require.Equal(t, "3: alpha again", scoped.(string))
}

// TestUnit_LocalFSBrowseTools_GrepDirectorySearchesRecursively asserts a
// directory path searches every file beneath it, formatting hits as
// "relpath:N: text".
func TestUnit_LocalFSBrowseTools_GrepDirectorySearchesRecursively(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc Needle() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("package sub\n\nfunc Other() {}\n"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, dt := browseExec(t, h, "grep", map[string]any{"path": ".", "pattern": "Needle"})
	require.Equal(t, taskengine.DataTypeString, dt)
	out := res.(string)
	require.Contains(t, out, "a.go:3: func Needle() {}")
	require.NotContains(t, out, "sub/b.go")
}

// TestUnit_LocalFSBrowseTools_GrepDirectorySkipsBinariesAndNoiseDirs asserts
// binary files and default high-noise directories never surface as matches.
func TestUnit_LocalFSBrowseTools_GrepDirectorySkipsBinariesAndNoiseDirs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "node_modules", "lib.js"), []byte("needle\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "blob.bin"), append([]byte("needle\x00"), make([]byte, 32)...), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("needle here\n"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _ := browseExec(t, h, "grep", map[string]any{"path": ".", "pattern": "needle"})
	out := res.(string)
	require.Contains(t, out, "keep.txt")
	require.NotContains(t, out, "node_modules")
	require.NotContains(t, out, "blob.bin")
}

// TestUnit_LocalFSBrowseTools_GrepDirectoryRegexMode asserts regex=true works
// across a directory search too.
func TestUnit_LocalFSBrowseTools_GrepDirectoryRegexMode(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("line one\nline two\nline three\n"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, _ := browseExec(t, h, "grep", map[string]any{"path": ".", "pattern": `^line \w+$`, "regex": true})
	out := res.(string)
	require.Contains(t, out, "a.txt:1: line one")
	require.Contains(t, out, "a.txt:2: line two")
	require.Contains(t, out, "a.txt:3: line three")
}

// TestUnit_LocalFSBrowseTools_GrepRefusesBinaryFile asserts a binary path is
// refused with a recoverable error rather than dumped into the transcript.
func TestUnit_LocalFSBrowseTools_GrepRefusesBinaryFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "blob.bin"), append([]byte("needle\x00"), make([]byte, 32)...), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	_, _, err := h.Exec(context.Background(), time.Now(), map[string]any{"path": "blob.bin", "pattern": "needle"}, false,
		&taskengine.ToolsCall{ToolName: "grep"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "binary file")
	require.Contains(t, err.Error(), "(recoverable:")
}

// TestUnit_LocalFSBrowseTools_FindFilesDoubleStarSpansAnyDepth asserts "**"
// matches zero or more intervening directories, including directly in the
// anchor directory itself.
func TestUnit_LocalFSBrowseTools_FindFilesDoubleStarSpansAnyDepth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "top.ts"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a", "mid.ts"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a", "b", "deep.ts"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a", "b", "deep.js"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	matches := findFilesMatches(t, h, map[string]any{"pattern": "src/**/*.ts"})
	require.ElementsMatch(t, []string{"src/top.ts", "src/a/mid.ts", "src/a/b/deep.ts"}, matches)
}

// TestUnit_LocalFSBrowseTools_FindFilesDoubleStarMidPatternAnchorsSuffix asserts
// a "**" not at the pattern's end still requires the literal suffix after it to
// match.
func TestUnit_LocalFSBrowseTools_FindFilesDoubleStarMidPatternAnchorsSuffix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "sub", "internal"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "sub", "internal", "thing_test.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "sub", "internal", "thing.go"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	matches := findFilesMatches(t, h, map[string]any{"pattern": "pkg/**/*_test.go"})
	require.Equal(t, []string{"pkg/sub/internal/thing_test.go"}, matches)
}

// TestUnit_LocalFSBrowseTools_FindFilesPlainGlobs is a regression guard:
// patterns without "**" keep basename/relative-path matching.
func TestUnit_LocalFSBrowseTools_FindFilesPlainGlobs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "subdir", "lib.go"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	matches := findFilesMatches(t, h, map[string]any{"pattern": "*.go"})
	require.ElementsMatch(t, []string{"main.go", "subdir/lib.go"}, matches)

	scoped := findFilesMatches(t, h, map[string]any{"pattern": "subdir/*.go"})
	require.Equal(t, []string{"subdir/lib.go"}, scoped)
}

// TestUnit_LocalFSBrowseTools_FindFilesHonoursGitignore asserts the hand-rolled
// matcher keeps ignored paths out of a listing.
func TestUnit_LocalFSBrowseTools_FindFilesHonoursGitignore(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "gen"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("gen/\n*.tmp\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "gen", "hidden.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "scratch.tmp"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kept.go"), []byte("x"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	require.Equal(t, []string{"kept.go"}, findFilesMatches(t, h, map[string]any{"pattern": "*.go"}))
	require.Empty(t, findFilesMatches(t, h, map[string]any{"pattern": "*.tmp"}))
}

// TestUnit_LocalFSBrowseTools_CountStats asserts the wc-style summary and that a
// directory is refused.
func TestUnit_LocalFSBrowseTools_CountStats(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("one two\nthree\n"), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, dt := browseExec(t, h, "count_stats", map[string]any{"path": "a.txt"})
	require.Equal(t, taskengine.DataTypeString, dt)
	require.Equal(t, "Lines: 2, Words: 3, Bytes: 14", res.(string))

	_, _, err := h.Exec(context.Background(), time.Now(), map[string]any{"path": "."}, false,
		&taskengine.ToolsCall{ToolName: "count_stats"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "is a directory")
}

// TestUnit_LocalFSBrowseTools_StatFile asserts the metadata shape, including the
// prefix-only binary sniff.
func TestUnit_LocalFSBrowseTools_StatFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "blob.bin"), append([]byte{0x00}, make([]byte, 16)...), 0o644))

	h := localtools.NewLocalFSBrowseTools(root, nil)
	res, dt := browseExec(t, h, "stat_file", map[string]any{"path": "run.sh"})
	require.Equal(t, taskengine.DataTypeJSON, dt)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.(string)), &got))
	require.Equal(t, "run.sh", got["name"])
	require.Equal(t, false, got["isDir"])
	require.Equal(t, true, got["executable"])
	require.Equal(t, false, got["binary"])

	bin, _ := browseExec(t, h, "stat_file", map[string]any{"path": "blob.bin"})
	require.NoError(t, json.Unmarshal([]byte(bin.(string)), &got))
	require.Equal(t, true, got["binary"])
	require.Equal(t, false, got["executable"])
}

// TestUnit_LocalFSBrowseTools_UnknownToolIsRefused asserts the browse toolset
// does not answer for the write half that still lives in local_fs.
func TestUnit_LocalFSBrowseTools_UnknownToolIsRefused(t *testing.T) {
	h := localtools.NewLocalFSBrowseTools(t.TempDir(), nil)
	for _, tool := range []string{"read_file", "write_file", "edit_file", "sed", "read_file_range"} {
		_, _, err := h.Exec(context.Background(), time.Now(), map[string]any{"path": "a.txt"}, false,
			&taskengine.ToolsCall{ToolName: tool})
		require.Error(t, err)
		require.Contains(t, err.Error(), fmt.Sprintf("unknown tool %s", tool))
	}
}

// TestUnit_LocalFSBrowseTools_PublishesSchemaForEveryDeclaredTool asserts the
// OpenAPI contract covers exactly the declared roster.
func TestUnit_LocalFSBrowseTools_PublishesSchemaForEveryDeclaredTool(t *testing.T) {
	ctx := context.Background()
	h := localtools.NewLocalFSBrowseTools(t.TempDir(), nil)

	docs, err := h.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	doc, ok := docs[localtools.LocalFSBrowseToolsName]
	require.True(t, ok, "the doc must be keyed by the gated toolset name")

	declared, err := h.GetToolsForToolsByName(ctx, localtools.LocalFSBrowseToolsName)
	require.NoError(t, err)
	require.Len(t, declared, 5)
	for _, tool := range declared {
		require.NotEmpty(t, tool.Function.Description, tool.Function.Name)
	}
	require.Len(t, doc.Components.Schemas, 2*len(declared))
}
