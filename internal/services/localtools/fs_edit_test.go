package localtools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// TestUnit_EditFile_SingleReplace asserts a unique old_string is replaced and the write lands on disk.
func TestUnit_EditFile_SingleReplace(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha bravo charlie\n")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "bravo", "new_string": "BRAVO",
	})
	require.NoError(t, err)
	ed, ok := res.(localtools.FsEditResult)
	require.True(t, ok, "expected FsEditResult, got %T", res)
	require.True(t, ed.Written)
	require.Equal(t, 1, ed.Replacements)
	require.Equal(t, "alpha bravo charlie\n", ed.OldText)
	require.Equal(t, "alpha BRAVO charlie\n", ed.NewText)

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha BRAVO charlie\n", string(got))
}

// TestUnit_EditFile_NotFoundNamesNextStepAndDoesNotMutate asserts a missing old_string leaves the file untouched and tells the model what to do next.
func TestUnit_EditFile_NotFoundNamesNextStepAndDoesNotMutate(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	original := "alpha bravo charlie\n"
	writeFile(t, dir, "a.txt", original)

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "delta", "new_string": "DELTA",
	})
	require.NoError(t, err)
	msg, ok := res.(string)
	require.True(t, ok, "not-found must be a soft string result, not an FsEditResult: got %T", res)
	require.Contains(t, msg, "old_string not found in")
	require.Contains(t, msg, "call read_file and retry with the exact current text")
	require.Contains(t, msg, "(recoverable:")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, original, string(got), "a no-match edit must never mutate the file")
}

// TestUnit_EditFile_AmbiguousWithoutReplaceAllNamesCountAndDoesNotMutate asserts a non-unique old_string is refused rather than guessed at.
func TestUnit_EditFile_AmbiguousWithoutReplaceAllNamesCountAndDoesNotMutate(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	original := "foo foo foo\n"
	writeFile(t, dir, "a.txt", original)

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "foo", "new_string": "bar",
	})
	require.NoError(t, err)
	msg, ok := res.(string)
	require.True(t, ok, "ambiguous edit must be a soft string result, not an FsEditResult: got %T", res)
	require.Contains(t, msg, "occurs 3 times")
	require.Contains(t, msg, "replace_all")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, original, string(got), "an ambiguous edit must never mutate the file")
}

// TestUnit_EditFile_ReplaceAll asserts replace_all rewrites every occurrence in one call.
func TestUnit_EditFile_ReplaceAll(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "foo foo foo\n")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "foo", "new_string": "bar", "replace_all": true,
	})
	require.NoError(t, err)
	ed, ok := res.(localtools.FsEditResult)
	require.True(t, ok, "expected FsEditResult, got %T", res)
	require.True(t, ed.Written)
	require.Equal(t, 3, ed.Replacements)

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "bar bar bar\n", string(got))
}

// TestUnit_EditFile_DeniedWithoutRead asserts edit_file enforces the same read-before-write gate as write_file/sed.
func TestUnit_EditFile_DeniedWithoutRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha bravo\n")

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "alpha", "new_string": "ALPHA",
	})
	require.NoError(t, err)
	msg := fsRefusalText(t, res)
	require.Contains(t, msg, "read_file")
	require.Contains(t, msg, "without reading it first")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha bravo\n", string(got), "edit_file must not have run when denied")
}

// TestUnit_EditFile_AllowedAfterRangeRead asserts a scoped read_file_range satisfies edit_file's gate, matching sed's targeted-mutator contract.
func TestUnit_EditFile_AllowedAfterRangeRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha\nbravo\ncharlie\n")

	_, err := execTool(t, ctx, tools, "read_file_range", map[string]any{
		"path": "a.txt", "start_line": float64(1), "end_line": float64(2),
	})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "alpha", "new_string": "ALPHA",
	})
	require.NoError(t, err)
	ed, ok := res.(localtools.FsEditResult)
	require.True(t, ok, "expected FsEditResult, got %T", res)
	require.True(t, ed.Written, "read_file_range must satisfy edit_file's read-before-write contract")
}

// TestUnit_EditFile_DeniedWhenFileChangedAfterRead asserts a stale read is refused rather than clobbering an external change.
func TestUnit_EditFile_DeniedWhenFileChangedAfterRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha bravo\n")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("external change\n"), 0644))

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "alpha", "new_string": "ALPHA",
	})
	require.NoError(t, err, "stale-read denial should be a soft tool result, not a chain error")
	msg := fsRefusalText(t, res)
	require.Contains(t, msg, "changed")
	require.Contains(t, msg, "read")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "external change\n", string(got), "stale edit must not overwrite external changes")
}

// TestUnit_EditFile_PathEscapeDenied asserts edit_file enforces the same containment as every other local_fs mutator.
func TestUnit_EditFile_PathEscapeDenied(t *testing.T) {
	ctx, tools, _ := setupFSReadGuard(t)

	_, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "/etc/passwd", "old_string": "root", "new_string": "toor",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "escapes allowed directory")
}

// TestUnit_EditFile_OldEqualsNewIsRejected asserts a no-op edit is refused rather than silently succeeding.
func TestUnit_EditFile_OldEqualsNewIsRejected(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha\n")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	_, err = execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "alpha", "new_string": "alpha",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "identical")
}

// TestUnit_EditFile_EmptyOldStringIsRejected asserts an empty old_string is refused before touching the file.
func TestUnit_EditFile_EmptyOldStringIsRejected(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha\n")

	_, err := execTool(t, ctx, tools, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "", "new_string": "alpha",
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "must not be empty"))
}
