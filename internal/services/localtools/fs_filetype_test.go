package localtools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// writeExecutableBinary creates dir/name containing size bytes of clearly
// non-text content (a repeating full-byte-range pattern, guaranteed to
// contain both NUL bytes and invalid UTF-8) with the executable bit set. It
// stands in for the defect report this file guards against: a 50 MB compiled
// executable that list_dir and stat_file could not distinguish from a
// directory or a text file.
func writeExecutableBinary(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 256)
	}
	require.NoError(t, os.WriteFile(p, content, 0755))
	return p
}

// TestFailure this guards against: before this fix, list_dir printed
// "contenox" and "main.go" as visually identical lines — nothing in the
// output told a model that one of them was a 50 MB executable rather than a
// directory or a source file. This asserts the ls -F-style annotations that
// close that gap: '/' for directories (already existed), '*' for the
// executable bit, and a compact size in parentheses once a file is large
// enough to matter.
func TestUnit_ListDir_AnnotatesExecutableAndLargeFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "pkgdir"), 0755))
	writeFile(t, dir, "main.go", "package main\n")
	writeExecutableBinary(t, dir, "contenox", 2*1024*1024) // 2 MiB: over the 1 MiB notice threshold

	h := localtools.NewLocalFSTools(dir, nil)
	res, err := execTool(t, context.Background(), h, "list_dir", map[string]any{"path": "."})
	require.NoError(t, err)
	lines := strings.Split(res.(string), "\n")

	require.True(t, contains(lines, "pkgdir/"), "directories keep the existing trailing slash: %v", lines)
	require.True(t, contains(lines, "main.go"), "a small non-executable text file gets no suffix at all: %v", lines)
	var contenoxLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "contenox") {
			contenoxLine = l
		}
	}
	require.Equal(t, "contenox* (2.0 MiB)", contenoxLine, "executable + oversized file gets a '*' and a compact size")
}

// A small executable text file (e.g. a shell script) must not be annotated
// with a size — only the '*' marks it. The size hint exists to flag files a
// model should think twice about read_file'ing, not to flag "any script".
func TestUnit_ListDir_SmallExecutableGetsNoSizeAnnotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0755))

	h := localtools.NewLocalFSTools(dir, nil)
	res, err := execTool(t, context.Background(), h, "list_dir", map[string]any{"path": "."})
	require.NoError(t, err)
	lines := strings.Split(res.(string), "\n")
	require.True(t, contains(lines, "run.sh*"), "executable bit alone still gets the '*' marker: %v", lines)
}

// The recursive listing must carry the same annotations as the non-recursive
// one — the walk path is a separate code path (walkListDir) and previously
// diverged easily.
func TestUnit_ListDir_RecursiveAnnotatesExecutableAndLargeFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0755))
	writeExecutableBinary(t, dir, filepath.Join("bin", "contenox"), 2*1024*1024)
	writeFile(t, dir, filepath.Join("bin", "README.md"), "docs\n")

	h := localtools.NewLocalFSTools(dir, nil)
	res, err := execTool(t, context.Background(), h, "list_dir", map[string]any{
		"path":      "bin",
		"recursive": true,
	})
	require.NoError(t, err)
	listing := res.(string)
	require.Contains(t, listing, "bin/contenox* (2.0 MiB)")
	require.Contains(t, listing, "bin/README.md\n")
}

// TestFailure this guards against: the live incident report — an agent tried
// list_dir on a path that turned out to be a 50 MB executable, and got back
// only "path must be a directory", with no hint that the path was in fact a
// giant binary. The error must describe what the path actually is.
func TestUnit_ListDir_NonDirectoryErrorDescribesWhatItIs(t *testing.T) {
	dir := t.TempDir()
	writeExecutableBinary(t, dir, "contenox", 2*1024*1024)

	h := localtools.NewLocalFSTools(dir, nil)
	_, err := execTool(t, context.Background(), h, "list_dir", map[string]any{"path": "contenox"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
	require.Contains(t, err.Error(), "regular file")
	require.Contains(t, err.Error(), "2.0 MiB")
	require.Contains(t, err.Error(), "executable")
	require.Contains(t, err.Error(), "binary")
}

// stat_file is the tool the agent in the incident fell back to, and its JSON
// never said "executable" or "binary" — only isDir:false and a raw byte
// count. This asserts the additive fields close that gap, and that ordinary
// files/directories are not falsely flagged.
func TestUnit_StatFile_ReportsExecutableAndBinaryFlags(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "pkgdir"), 0755))
	writeFile(t, dir, "main.go", "package main\n")
	writeExecutableBinary(t, dir, "contenox", 2*1024*1024)

	h := localtools.NewLocalFSTools(dir, nil)

	type statResult struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		SizeHuman  string `json:"sizeHuman"`
		IsDir      bool   `json:"isDir"`
		Mode       string `json:"mode"`
		Executable bool   `json:"executable"`
		Binary     bool   `json:"binary"`
	}

	statOf := func(path string) statResult {
		res, err := execTool(t, context.Background(), h, "stat_file", map[string]any{"path": path})
		require.NoError(t, err)
		var out statResult
		require.NoError(t, json.Unmarshal([]byte(res.(string)), &out))
		return out
	}

	dirStat := statOf("pkgdir")
	require.True(t, dirStat.IsDir)
	require.False(t, dirStat.Executable, "a directory's traversal bit must not be reported as file-executable")
	require.False(t, dirStat.Binary)

	textStat := statOf("main.go")
	require.False(t, textStat.IsDir)
	require.False(t, textStat.Executable)
	require.False(t, textStat.Binary, "a plain Go source file must not sniff as binary")

	binStat := statOf("contenox")
	require.False(t, binStat.IsDir)
	require.True(t, binStat.Executable, "0755 regular file must report executable:true")
	require.True(t, binStat.Binary, "content full of NUL/high bytes must sniff as binary")
	require.Equal(t, "2.0 MiB", binStat.SizeHuman)
	require.True(t, strings.HasPrefix(binStat.Mode, "-rwx"), "mode string should read like ls -l: %q", binStat.Mode)
}

// stat_file's binary sniff must stay cheap: it should classify a file far
// larger than any read_file size policy without loading the whole thing.
// This is a behavioral proxy for that — it uses a size well above the
// default _max_read_bytes (1 MiB) that read_file would refuse outright, and
// confirms stat_file still answers instead of erroring.
func TestUnit_StatFile_SniffIsCheapOnLargeFiles(t *testing.T) {
	dir := t.TempDir()
	writeExecutableBinary(t, dir, "contenox", 8*1024*1024) // 8 MiB, well over read_file's default cap

	h := localtools.NewLocalFSTools(dir, nil)
	res, err := execTool(t, context.Background(), h, "stat_file", map[string]any{"path": "contenox"})
	require.NoError(t, err)
	require.Contains(t, res.(string), `"binary":true`)
	require.Contains(t, res.(string), `"executable":true`)
}

// TestFailure this guards against: read_file on a binary file used to just
// dump raw bytes into the model's transcript as a Go string — wasted tokens
// on content with no textual meaning, and no explanation of what happened.
func TestUnit_ReadFile_RefusesBinaryWithTeachingError(t *testing.T) {
	dir := t.TempDir()
	// Small enough to pass the default _max_read_bytes gate untouched, so
	// this specifically exercises the new content-based refusal rather than
	// the pre-existing size guard.
	writeExecutableBinary(t, dir, "contenox", 4096)

	h := localtools.NewLocalFSTools(dir, nil)
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "contenox"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to read")
	require.Contains(t, err.Error(), "binary")
	require.Contains(t, err.Error(), "executable")
	require.Contains(t, err.Error(), "stat_file")
}

// An executable that is nonetheless plain text (a shell script) must still
// be readable — the refusal is about content, not the executable bit.
func TestUnit_ReadFile_ExecutableTextFileIsStillReadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.sh")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0755))

	h := localtools.NewLocalFSTools(dir, nil)
	res, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "run.sh"})
	require.NoError(t, err)
	require.Contains(t, res.(string), "echo hi")
}

// Item 3 of the fix ("if it already guards, verify + test it"): a binary
// large enough to exceed the default _max_read_bytes policy (1 MiB) must
// still be blocked on read_file, via the pre-existing size gate that runs
// before content is ever loaded — the new binary-content check never gets a
// chance to run, and that is fine, since the file is refused either way.
func TestUnit_ReadFile_OversizedBinaryStillBlockedBySizeGuard(t *testing.T) {
	dir := t.TempDir()
	writeExecutableBinary(t, dir, "contenox", 2*1024*1024) // over the 1 MiB default cap

	h := localtools.NewLocalFSTools(dir, nil)
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "contenox"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "binary")
}
