package localtools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupFSReadGuard(t *testing.T) (context.Context, taskengine.ToolsRepo, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	allowedDir := t.TempDir()
	tools := localtools.NewLocalFSTools(allowedDir, db)
	ctxWithSession := context.WithValue(ctx, runtimetypes.SessionIDContextKey, "test-session")
	return ctxWithSession, tools, allowedDir
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0644))
	return p
}

func execTool(t *testing.T, ctx context.Context, h taskengine.ToolsRepo, name string, args map[string]any) (any, error) {
	t.Helper()
	res, _, err := h.Exec(ctx, time.Now(), args, false, &taskengine.ToolsCall{ToolName: name})
	return res, err
}

func TestUnit_ReadBeforeWrite_AllowedAfterRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "original")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "a.txt", "content": "updated"})
	require.NoError(t, err)
	fw, ok := res.(localtools.FsWriteResult)
	require.True(t, ok, "expected FsWriteResult, got %T", res)
	require.True(t, fw.Written)
	require.Equal(t, "original", fw.OldText)
	require.Equal(t, "updated", fw.NewText)

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "updated", string(got))
}

func TestUnit_ReadBeforeWrite_DeniedWithoutRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "original")

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "a.txt", "content": "updated"})
	require.NoError(t, err, "denial must be a soft string result, not a chain error")
	msg, ok := res.(string)
	require.True(t, ok, "denial result must be string, got %T", res)
	require.Contains(t, msg, "read_file")
	require.Contains(t, msg, "without reading it first")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "original", string(got), "file must not have been mutated when denied")
}

func TestUnit_ReadBeforeWrite_NewFileAllowed(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "new.txt", "content": "fresh"})
	require.NoError(t, err)
	fw, ok := res.(localtools.FsWriteResult)
	require.True(t, ok, "creating a new file should not require a prior read; got %T", res)
	require.True(t, fw.Written)
	require.Empty(t, fw.OldText)
	require.Equal(t, "fresh", fw.NewText)

	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "fresh", string(got))
}

func TestUnit_ReadBeforeWrite_SedDeniedWithoutRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha bravo")

	res, err := execTool(t, ctx, tools, "sed", map[string]any{
		"path":        "a.txt",
		"pattern":     "alpha",
		"replacement": "ALPHA",
	})
	require.NoError(t, err)
	msg, ok := res.(string)
	require.True(t, ok)
	require.Contains(t, msg, "read_file")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "alpha bravo", string(got), "sed must not have run when denied")
}

func TestUnit_ReadBeforeWrite_SedAllowedAfterRangeRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha\nbravo\ncharlie")

	_, err := execTool(t, ctx, tools, "read_file_range", map[string]any{
		"path":       "a.txt",
		"start_line": float64(1),
		"end_line":   float64(2),
	})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "sed", map[string]any{
		"path":        "a.txt",
		"pattern":     "alpha",
		"replacement": "ALPHA",
	})
	require.NoError(t, err)
	sed, ok := res.(localtools.FsSedResult)
	require.True(t, ok, "expected FsSedResult, got %T", res)
	require.True(t, sed.Written, "read_file_range must satisfy the read-before-write contract")
	require.Equal(t, 1, sed.Replacements)

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Contains(t, string(got), "ALPHA")
}

func TestUnit_ReadBeforeWrite_BypassWithoutSession(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "original")
	tools := localtools.NewLocalFSTools(dir, db)

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "a.txt", "content": "updated"})
	require.NoError(t, err)
	fw, ok := res.(localtools.FsWriteResult)
	require.True(t, ok, "without a session ID the guard must fall open; got %T", res)
	require.True(t, fw.Written)
}

func TestUnit_ReadBeforeWrite_NilDBBypasses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "original")

	tools := localtools.NewLocalFSTools(dir, nil)
	ctx := context.WithValue(context.Background(), runtimetypes.SessionIDContextKey, "irrelevant")

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "a.txt", "content": "updated"})
	require.NoError(t, err)
	fw, ok := res.(localtools.FsWriteResult)
	require.True(t, ok, "nil db must disable the guard; got %T", res)
	require.True(t, fw.Written)
}

func TestUnit_ReadBeforeWrite_PathNormalization(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "original")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	absPath := filepath.Join(dir, "a.txt")
	res, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": absPath, "content": "updated"})
	require.NoError(t, err)
	fw, ok := res.(localtools.FsWriteResult)
	require.True(t, ok, "absolute and relative paths must canonicalize to the same key; got %T", res)
	require.True(t, fw.Written)
}

func TestUnit_ReadBeforeWrite_SessionScoping(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "original")
	tools := localtools.NewLocalFSTools(dir, db)

	ctxA := context.WithValue(ctx, runtimetypes.SessionIDContextKey, "session-A")
	_, err = execTool(t, ctxA, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	ctxB := context.WithValue(ctx, runtimetypes.SessionIDContextKey, "session-B")
	res, err := execTool(t, ctxB, tools, "write_file", map[string]any{"path": "a.txt", "content": "updated"})
	require.NoError(t, err)
	msg, _ := res.(string)
	require.Contains(t, msg, "without reading it first", "a read in session A must not satisfy a write in session B")
}

func TestUnit_ReadBeforeWrite_DeniedWhenFileChangedAfterRead(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "original\n")

	_, err := execTool(t, ctx, tools, "read_file", map[string]any{
		"path": "a.txt",
	})
	require.NoError(t, err)

	// Simulate the filesystem changing after the agent read the file.
	// A guard that only stores "this path was read" will incorrectly allow
	// the next write and overwrite this external change.
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "a.txt"),
		[]byte("external change\n"),
		0644,
	))

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{
		"path":    "a.txt",
		"content": "agent overwrite\n",
	})
	require.NoError(t, err, "stale-read denial should be a soft tool result, not a chain error")

	msg, ok := res.(string)
	require.True(t, ok, "expected stale-read denial as string, got %T: %#v", res, res)
	require.Contains(t, msg, "changed", "denial should explain that the file changed since it was read")
	require.Contains(t, msg, "read", "denial should tell the agent to re-read before writing")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "external change\n", string(got), "stale write must not overwrite external changes")
}

func TestUnit_ReadBeforeWrite_RangeReadDoesNotAuthorizeFullFileWrite(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)

	original := strings.Join([]string{
		"line 1: alpha",
		"line 2: bravo",
		"line 3: charlie",
		"line 4: delta",
		"line 5: echo",
	}, "\n") + "\n"

	writeFile(t, dir, "a.txt", original)

	_, err := execTool(t, ctx, tools, "read_file_range", map[string]any{
		"path":       "a.txt",
		"start_line": float64(1),
		"end_line":   float64(2),
	})
	require.NoError(t, err)

	// A range read should not authorize replacing the whole file.
	// Otherwise the agent can inspect two lines and then destroy unseen content.
	res, err := execTool(t, ctx, tools, "write_file", map[string]any{
		"path":    "a.txt",
		"content": "collapsed rewrite\n",
	})
	require.NoError(t, err, "range-read denial should be a soft tool result, not a chain error")

	msg, ok := res.(string)
	require.True(t, ok, "expected range-read denial as string, got %T: %#v", res, res)
	require.Contains(t, msg, "read_file", "denial should tell the agent to read the full file before full overwrite")
	require.Contains(t, msg, "range", "denial should explain that a range read is insufficient for full-file write")

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, original, string(got), "full-file write after range read must not mutate the file")
}

func TestUnit_ReadBeforeWrite_InvalidationAfterMutation(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "line1\nline2\n")

	// 1) Read, then write – allowed
	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "write_file", map[string]any{
		"path":    "a.txt",
		"content": "changed\n",
	})
	require.NoError(t, err)
	fw, ok := res.(localtools.FsWriteResult)
	require.True(t, ok)
	require.True(t, fw.Written)

	// 2) Immediately try another write without a new read – should be denied
	res, err = execTool(t, ctx, tools, "write_file", map[string]any{
		"path":    "a.txt",
		"content": "changed again\n",
	})
	require.NoError(t, err, "denial should be a soft tool result")
	msg, ok := res.(string)
	require.True(t, ok, "expected denial message string, got %T", res)
	require.Contains(t, msg, "without reading it first")

	// File should still contain the first mutation, not the second
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "changed\n", string(got))
}

func TestUnit_ReadBeforeWrite_SedInvalidation(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "alpha bravo\n")

	// read then sed – allowed
	_, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	res, err := execTool(t, ctx, tools, "sed", map[string]any{
		"path":        "a.txt",
		"pattern":     "alpha",
		"replacement": "ALPHA",
	})
	require.NoError(t, err)
	sed, ok := res.(localtools.FsSedResult)
	require.True(t, ok, "expected FsSedResult, got %T", res)
	require.True(t, sed.Written)
	require.Equal(t, 1, sed.Replacements)

	// Second sed without re-read – denied
	res, err = execTool(t, ctx, tools, "sed", map[string]any{
		"path":        "a.txt",
		"pattern":     "bravo",
		"replacement": "BRAVO",
	})
	require.NoError(t, err)
	msg, ok := res.(string)
	require.True(t, ok)
	require.Contains(t, msg, "without reading it first")

	// Content unchanged from first sed
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "ALPHA bravo\n", string(got))
}
