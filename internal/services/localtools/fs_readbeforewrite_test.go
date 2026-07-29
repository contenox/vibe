package localtools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// fsRefusalText renders a soft REFUSAL the way the engine renders it for the
// model: a DataTypeString result goes through fmt.Sprintf("%v", …)
// (taskengine/taskexec.go, serializeToolResultContent), so a typed refusal whose
// String() is the denial sentence reaches the model as exactly the bytes it
// always did.
//
// The type exists so a PROGRAM cannot read the apology as a receipt: a
// FsRefusalResult declares itself unusable, and the goja bridge throws instead of
// handing a script a sentence about a write that never happened. This helper
// asserts both halves — the text is unchanged, and the value is the typed
// refusal rather than a bare string.
func fsRefusalText(t *testing.T, res any) string {
	t.Helper()
	refusal, ok := res.(localtools.FsRefusalResult)
	require.Truef(t, ok, "expected a typed FsRefusalResult a program cannot mistake for a result, got %T: %#v", res, res)
	require.True(t, refusal.Refused)
	require.Equal(t, refusal.Reason, refusal.String(), "the model-facing rendering must be the denial sentence itself")
	require.NotEmpty(t, refusal.ProgramUnusable(), "a refusal must declare itself unusable to a program")
	return refusal.String()
}

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
	msg := fsRefusalText(t, res)
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
	msg := fsRefusalText(t, res)
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
	msg := fsRefusalText(t, res)
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

	msg := fsRefusalText(t, res)
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

	msg := fsRefusalText(t, res)
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
	msg := fsRefusalText(t, res)
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
	msg := fsRefusalText(t, res)
	require.Contains(t, msg, "without reading it first")

	// Content unchanged from first sed
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "ALPHA bravo\n", string(got))
}

// TestUnit_ReadFile_UnchangedStubIsAStandInForAReaderOnly is the regression test
// for the second live failure (2026-07-27): a goja script called read_file, got
// the session-dedup stub, and treated "File unchanged since last read — the
// content from your earlier read_file call in this conversation is still
// current." as the file's content.
//
// The sentence is TRUE of a model, whose earlier read is still in its context.
// It is false of every caller that never made that read. So the stub is now a
// typed value: it renders to the model as the identical sentence (the dedup is
// untouched, and those tokens still never leave), and it hands a PROGRAM the
// content it was standing in for.
func TestUnit_ReadFile_UnchangedStubIsAStandInForAReaderOnly(t *testing.T) {
	ctx, tools, dir := setupFSReadGuard(t)
	writeFile(t, dir, "a.txt", "line one\nline two\n")

	first, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)
	require.Equal(t, "line one\nline two\n", first, "the first read is the file itself")

	second, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	stub, ok := second.(localtools.FsUnchangedResult)
	require.Truef(t, ok, "a repeat read answered %T; a bare sentence is indistinguishable from content to a program", second)

	// The MODEL still sees exactly the sentence it always did — the dedup is the
	// point, and the engine renders a DataTypeString result with %v.
	require.Equal(t,
		"File unchanged since last read — the content from your earlier read_file call in this conversation is still current. Pass force=true if you need the content re-sent.",
		stub.String(), "the model-facing dedup message changed")

	// A PROGRAM gets the file.
	text, available := stub.ProgramText()
	require.True(t, available, "the stub must be redeemable for the content it stands in for")
	require.Equal(t, "line one\nline two\n", text,
		"a program asked for a file and was handed a sentence about a conversation it is not in")

	require.True(t, stub.Unchanged)
	require.Equal(t, len("line one\nline two\n"), stub.Bytes)
	require.NotEmpty(t, stub.SHA256)

	// force still works, unchanged, for the caller that wants the bytes resent
	// into the model's context.
	forced, err := execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt", "force": true})
	require.NoError(t, err)
	require.Equal(t, "line one\nline two\n", forced)
}
