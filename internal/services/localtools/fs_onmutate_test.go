package localtools_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// setupFSMutateGuard is setupFSReadGuard plus a recorder wired via WithOnFileMutated.
func setupFSMutateGuard(t *testing.T) (context.Context, taskengine.ToolsRepo, string, *[]string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var mutated []string
	allowedDir := t.TempDir()
	tools := localtools.NewLocalFSToolsWith(allowedDir, db, nil, localtools.LocalFSToolsName, nil,
		localtools.WithOnFileMutated(func(absPath string) { mutated = append(mutated, absPath) }))
	ctxWithSession := context.WithValue(ctx, runtimetypes.SessionIDContextKey, "test-session")
	return ctxWithSession, tools, allowedDir, &mutated
}

// TestUnit_OnFileMutated_FiresForEveryMutatingTool asserts the callback fires
// exactly once per successful write_file/sed/edit_file, naming the absolute
// path, and not at all for a read-before-write denial.
func TestUnit_OnFileMutated_FiresForEveryMutatingTool(t *testing.T) {
	ctx, tools, dir, mutated := setupFSMutateGuard(t)
	abs := filepath.Join(dir, "a.txt")

	// write_file on a brand-new file needs no prior read.
	_, err := execTool(t, ctx, tools, "write_file", map[string]any{"path": "a.txt", "content": "alpha bravo\n"})
	require.NoError(t, err)
	require.Equal(t, []string{abs}, *mutated)

	_, err = execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	_, err = execTool(t, ctx, tools, "sed", map[string]any{"path": "a.txt", "pattern": "alpha", "replacement": "ALPHA"})
	require.NoError(t, err)
	require.Equal(t, []string{abs, abs}, *mutated)

	_, err = execTool(t, ctx, tools, "read_file", map[string]any{"path": "a.txt"})
	require.NoError(t, err)

	_, err = execTool(t, ctx, tools, "edit_file", map[string]any{"path": "a.txt", "old_string": "bravo", "new_string": "BRAVO"})
	require.NoError(t, err)
	require.Equal(t, []string{abs, abs, abs}, *mutated)
}

// TestUnit_OnFileMutated_DoesNotFireOnDenial asserts a read-before-write
// refusal never invokes the callback — nothing was actually written.
func TestUnit_OnFileMutated_DoesNotFireOnDenial(t *testing.T) {
	ctx, tools, dir, mutated := setupFSMutateGuard(t)
	writeFile(t, dir, "a.txt", "alpha\n")

	res, err := execTool(t, ctx, tools, "edit_file", map[string]any{"path": "a.txt", "old_string": "alpha", "new_string": "ALPHA"})
	require.NoError(t, err)
	fsRefusalText(t, res)
	require.Empty(t, *mutated, "a denied mutation must not fire the on-file-mutated callback")
}
