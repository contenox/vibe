package localtools_test

// End-to-end tests, driven through the real LocalFSTools.Exec pipeline:
//
//   - read_file never truncates silently: it returns a bounded head plus a
//     concrete "start_line: N" next step; paging from that number continues.
//   - the recoverable/fatal severity marker rides on the matrix of
//     correctable error paths.
//   - missing paths suggest siblings; a sed no-match suggests the nearest
//     lines and never mutates.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

var startLineRe = regexp.MustCompile(`start_line:\s*(\d+)`)

// TestUnit_ReadFile_TruncateNamesExactNextLineAndPages pins that a truncated read names the exact next line, and paging continues cleanly.
func TestUnit_ReadFile_TruncateNamesExactNextLineAndPages(t *testing.T) {
	dir := t.TempDir()
	// Six 6-char lines, no trailing newline.
	writeFile(t, dir, "big.txt", "line01\nline02\nline03\nline04\nline05\nline06")

	h := localtools.NewLocalFSTools(dir, nil)
	// _max_output_bytes=20 fits exactly three lines ("line01\nline02\nline03").
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{
		"_max_output_bytes": "20",
	})

	res, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "big.txt"})
	require.NoError(t, err)
	page1 := res.(string)

	require.Contains(t, page1, "line01")
	require.Contains(t, page1, "line03")
	require.NotContains(t, page1, "line04", "truncated head must not contain later lines")
	require.Contains(t, page1, "truncated")
	require.Contains(t, page1, "of 6", "notice should report the real total line count")

	m := startLineRe.FindStringSubmatch(page1)
	require.Len(t, m, 2, "notice must name a concrete start_line: %q", page1)
	next, _ := strconv.Atoi(m[1])
	require.Equal(t, 4, next, "next page must resume at the first unshown line")

	// Page forward with the named number.
	res2, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "big.txt", "start_line": float64(next)})
	require.NoError(t, err)
	page2 := res2.(string)
	require.Contains(t, page2, "line04")
	require.Contains(t, page2, "line06")
	require.NotContains(t, page2, "line01", "page 2 must not repeat page 1")
}

// TestUnit_ReadFile_OverReadCapNamesNextStep pins that a file over the read cap is refused, naming the range-read next step.
func TestUnit_ReadFile_OverReadCapNamesNextStep(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "huge.txt", strings.Repeat("abcdefgh\n", 300)) // ~2.6 KiB

	h := localtools.NewLocalFSTools(dir, nil)
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{
		"_max_read_bytes": "100",
	})
	_, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "huge.txt"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "read cap")
	require.Contains(t, err.Error(), "start_line")
	require.Contains(t, err.Error(), "(recoverable:")

	// The named next step actually works: a ranged read streams past the cap.
	res, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "huge.txt", "start_line": float64(1), "end_line": float64(2)})
	require.NoError(t, err, "ranged read must stream past the read cap")
	require.Equal(t, "abcdefgh\nabcdefgh", res.(string))
}

// TestUnit_Severity_RecoverableMatrix pins that every correctable error path is marked recoverable, never fatal.
func TestUnit_Severity_RecoverableMatrix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "text.txt", "hello\n")
	// A binary file for the binary-refusal path.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), append([]byte("\x00\x01\x02"), make([]byte, 100)...), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))

	h := localtools.NewLocalFSTools(dir, nil)
	ctx := context.Background()

	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"missing file", "read_file", map[string]any{"path": "nope.txt"}},
		{"escape", "read_file", map[string]any{"path": "/etc/passwd"}},
		{"binary refusal", "read_file", map[string]any{"path": "blob.bin"}},
		{"list on a file", "list_dir", map[string]any{"path": "text.txt"}},
		{"read a directory", "read_file", map[string]any{"path": "sub"}},
		{"missing stat", "stat_file", map[string]any{"path": "gone.txt"}},
		{"unknown arg", "read_file", map[string]any{"path": "text.txt", "bogus": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := execTool(t, ctx, h, c.tool, c.args)
			require.Error(t, err)
			require.Contains(t, err.Error(), "(recoverable:", "error must be tagged recoverable: %v", err)
			require.NotContains(t, err.Error(), "(fatal:", "correctable errors must not be fatal: %v", err)
		})
	}
}

// TestUnit_Severity_DenialIsRecoverable pins that a read-before-write denial is a soft, recoverable result, not a fatal error.
func TestUnit_Severity_DenialIsRecoverable(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "original\n")

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "a.txt", "content": "new"})
	require.NoError(t, err)
	msg := fsRefusalText(t, res)
	require.Contains(t, msg, "without reading it first")
	require.Contains(t, msg, "(recoverable:")
}

// TestUnit_DidYouMean_SuggestsSiblings pins that a missing path suggests similar sibling names.
func TestUnit_DidYouMean_SuggestsSiblings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "docs\n")
	writeFile(t, dir, "main.go", "package main\n")

	h := localtools.NewLocalFSTools(dir, nil)
	ctx := context.Background()

	for _, tool := range []string{"read_file", "stat_file"} {
		_, err := execTool(t, ctx, h, tool, map[string]any{"path": "readme.md"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Did you mean", "%s should suggest siblings", tool)
		require.Contains(t, err.Error(), "README.md", "%s should name the close sibling", tool)
	}

	// list_dir on a missing directory also suggests.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "internal"), 0o755))
	_, err := execTool(t, ctx, h, "list_dir", map[string]any{"path": "internl"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Did you mean")
	require.Contains(t, err.Error(), "internal")
}

// TestUnit_Sed_NoMatchSuggestsAndDoesNotMutate pins that a no-match sed suggests the nearest lines and never mutates the file.
func TestUnit_Sed_NoMatchSuggestsAndDoesNotMutate(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	original := "func Alpha() {}\nfunc Bravo() {}\nfunc Charlie() {}\n"
	writeFile(t, dir, "code.go", original)

	// Satisfy the read-before-write contract first.
	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "code.go"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "sed", map[string]any{
		"path": "code.go", "pattern": "func Bravl() {}", "replacement": "func Bravo2() {}",
	})
	require.NoError(t, err)

	msg, ok := res.(string)
	require.True(t, ok, "no-match sed must return a suggestion string, not an FsSedResult: got %T", res)
	require.Contains(t, msg, "not found")
	require.Contains(t, msg, "Closest lines:")
	require.Contains(t, msg, "Bravo", "should suggest the nearest actual line")
	require.Contains(t, msg, "(recoverable:")

	// The fuzzy law: nothing was applied.
	got, err := os.ReadFile(filepath.Join(dir, "code.go"))
	require.NoError(t, err)
	require.Equal(t, original, string(got), "a fuzzy no-match must never mutate the file")
}
