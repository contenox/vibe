package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func setupRemoteProviderACPDB(t *testing.T) (context.Context, libdb.DBManager, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "remoteprovider-acp.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db, runtimetypes.New(db.WithoutTransaction())
}

func testProviderMCP(name string) *runtimetypes.MCPServer {
	return &runtimetypes.MCPServer{
		Name:                  name,
		Transport:             "stdio",
		Command:               "echo",
		Args:                  []string{"ok"},
		ConnectTimeoutSeconds: 30,
	}
}

func TestUnit_PersistentRepo_HidesACPManagedMCPServersWithoutRuntimeAllowlist(t *testing.T) {
	ctx, db, store := setupRemoteProviderACPDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("durable")))
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("acp-current")))
	require.NoError(t, store.CreateMCPServer(ctx, testProviderMCP("acp-other")))

	repo := NewPersistentRepo(nil, db, nil, libbus.NewInMem(), nil)

	names, err := repo.Supports(ctx)
	require.NoError(t, err)
	require.Contains(t, names, "durable")
	require.NotContains(t, names, "acp-current")
	require.NotContains(t, names, "acp-other")

	scopedCtx := taskengine.WithRuntimeToolsAllowlist(ctx, []string{"*", "!acp-other"})
	names, err = repo.Supports(scopedCtx)
	require.NoError(t, err)
	require.Contains(t, names, "durable")
	require.Contains(t, names, "acp-current")
	require.NotContains(t, names, "acp-other")
}
