package acpsvc

import (
	"context"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupResolverDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.TODO()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return ctx, db
}

func TestUnit_ResolveSessionWorkspace_FindsSessionRegardlessOfPlacementWorkspace(t *testing.T) {
	ctx, db := setupResolverDB(t)

	const sessionID = "sess-1778834208786461947"
	const storedWorkspace = "acp-client-0b9a4d57f597718a"

	store := runtimetypes.NewMessageStore(db.WithoutTransaction(), storedWorkspace)
	require.NoError(t, store.CreateNamedMessageIndex(ctx, "idx-1", "acp-client", sessionID))

	tr := &Transport{deps: Deps{DB: db, WorkspaceID: "some-other-current-workspace"}}

	got, ok := tr.resolveSessionWorkspace(ctx, sessionID)
	require.True(t, ok, "a session that exists under any workspace must be locatable by its globally-unique ACP session ID, even when the transport's current placement workspace differs (restart, client change); otherwise it is silently orphaned and 'session not found'")
	require.Equal(t, storedWorkspace, got)
}

func TestUnit_ResolveSessionWorkspace_UnknownSessionReturnsFalse(t *testing.T) {
	ctx, db := setupResolverDB(t)
	tr := &Transport{deps: Deps{DB: db, WorkspaceID: "w"}}

	_, ok := tr.resolveSessionWorkspace(ctx, "sess-does-not-exist")
	require.False(t, ok)

	_, ok = tr.resolveSessionWorkspace(ctx, "")
	require.False(t, ok)
}
