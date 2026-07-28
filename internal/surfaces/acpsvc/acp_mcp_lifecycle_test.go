package acpsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

func setupACPMCPTestDB(t *testing.T) (context.Context, libdb.DBManager, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "acp-mcp.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db, runtimetypes.New(db.WithoutTransaction())
}

func testMCPServer(name string) *runtimetypes.MCPServer {
	return &runtimetypes.MCPServer{
		Name:                  name,
		Transport:             "stdio",
		Command:               "echo",
		Args:                  []string{"ok"},
		ConnectTimeoutSeconds: 30,
	}
}

func TestUnit_ACPMCPName_IsConnectionAndSessionScoped(t *testing.T) {
	a := mcpNameFor("conn-a", libacp.SessionID("sess-1"), "Filesystem MCP!")
	b := mcpNameFor("conn-b", libacp.SessionID("sess-1"), "Filesystem MCP!")
	c := mcpNameFor("conn-a", libacp.SessionID("sess-2"), "Filesystem MCP!")

	require.True(t, runtimetypes.IsACPManagedMCPServerName(a))
	require.NotEqual(t, a, b)
	require.NotEqual(t, a, c)
	require.LessOrEqual(t, len(a), 64)
	require.NotContains(t, a, " ")
	require.Contains(t, a, "filesystem_mcp")
}

func TestUnit_ACPRuntimeToolsAllowlist_ExcludesOtherACPServers(t *testing.T) {
	ctx, _, store := setupACPMCPTestDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testMCPServer("durable")))
	require.NoError(t, store.CreateMCPServer(ctx, testMCPServer("acp-current")))
	require.NoError(t, store.CreateMCPServer(ctx, testMCPServer("acp-other")))

	tr := &Transport{}
	allowlist, err := tr.runtimeToolsAllowlist(ctx, store, []string{"acp-current"})
	require.NoError(t, err)

	require.Contains(t, allowlist, "*")
	require.Contains(t, allowlist, "!acp-other")
	require.NotContains(t, allowlist, "!acp-current")
	require.NotContains(t, allowlist, "!durable")
}

func TestUnit_CleanupStaleACPManagedMCPServers_RemovesRowsAndSessionKV(t *testing.T) {
	ctx, db, store := setupACPMCPTestDB(t)
	require.NoError(t, store.CreateMCPServer(ctx, testMCPServer("durable")))
	require.NoError(t, store.CreateMCPServer(ctx, testMCPServer("acp-stale")))
	require.NoError(t, store.SetKV(ctx, "mcp_session:acp-stale:chat-1", json.RawMessage(`"sid-stale"`)))
	require.NoError(t, store.SetKV(ctx, "mcp_session:durable:chat-1", json.RawMessage(`"sid-durable"`)))

	require.NoError(t, CleanupStaleACPManagedMCPServers(ctx, db))

	_, err := store.GetMCPServerByName(ctx, "acp-stale")
	require.Error(t, err)
	_, err = store.GetMCPServerByName(ctx, "durable")
	require.NoError(t, err)

	var got string
	require.Error(t, store.GetKV(ctx, "mcp_session:acp-stale:chat-1", &got))
	require.NoError(t, store.GetKV(ctx, "mcp_session:durable:chat-1", &got))
	require.Equal(t, "sid-durable", got)
}
