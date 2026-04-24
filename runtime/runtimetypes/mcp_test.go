package runtimetypes_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func newSSE(name string) *runtimetypes.MCPServer {
	return &runtimetypes.MCPServer{
		ID:                    uuid.New().String(),
		Name:                  name,
		Transport:             "sse",
		URL:                   "http://mcp.example.com/sse",
		ConnectTimeoutSeconds: 30,
	}
}

func newStdio(name string) *runtimetypes.MCPServer {
	return &runtimetypes.MCPServer{
		ID:                    uuid.New().String(),
		Name:                  name,
		Transport:             "stdio",
		Command:               "npx",
		Args:                  []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		ConnectTimeoutSeconds: 30,
	}
}

// ─── CRUD ──────────────────────────────────────────────────────────────────────

func TestUnit_MCPServers_CreateAndGet(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("create-and-get")
	srv.AuthType = "bearer"
	srv.AuthEnvKey = "MY_MCP_TOKEN"

	require.NoError(t, s.CreateMCPServer(ctx, srv))

	// Get by ID
	got, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	require.Equal(t, srv.ID, got.ID)
	require.Equal(t, srv.Name, got.Name)
	require.Equal(t, srv.Transport, got.Transport)
	require.Equal(t, srv.URL, got.URL)
	require.Equal(t, srv.AuthType, got.AuthType)
	require.Equal(t, srv.AuthEnvKey, got.AuthEnvKey)
	require.Equal(t, srv.ConnectTimeoutSeconds, got.ConnectTimeoutSeconds)
	require.WithinDuration(t, time.Now().UTC(), got.CreatedAt, 2*time.Second)
	require.WithinDuration(t, time.Now().UTC(), got.UpdatedAt, 2*time.Second)

	// Get by name
	byName, err := s.GetMCPServerByName(ctx, srv.Name)
	require.NoError(t, err)
	require.Equal(t, srv.ID, byName.ID)
}

func TestUnit_MCPServers_ArgsRoundTrip(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newStdio("stdio-args-roundtrip")
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	got, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	require.Equal(t, srv.Command, got.Command)
	require.Equal(t, srv.Args, got.Args)
}

func TestUnit_MCPServers_NilArgs(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("nil-args")
	srv.Args = nil
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	got, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	// nil args should round-trip as nil (or empty slice — both acceptable)
	require.True(t, len(got.Args) == 0, "expected empty/nil args")
}

func TestUnit_MCPServers_Update(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	original := newSSE("update-me")
	require.NoError(t, s.CreateMCPServer(ctx, original))

	updated := *original
	updated.URL = "http://new-mcp.example.com/sse"
	updated.ConnectTimeoutSeconds = 60
	updated.AuthType = "bearer"
	updated.AuthToken = "literal-token"

	require.NoError(t, s.UpdateMCPServer(ctx, &updated))

	got, err := s.GetMCPServer(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, "http://new-mcp.example.com/sse", got.URL)
	require.Equal(t, 60, got.ConnectTimeoutSeconds)
	require.Equal(t, "bearer", got.AuthType)
	require.Equal(t, "literal-token", got.AuthToken)
	require.True(t, got.UpdatedAt.After(original.UpdatedAt), "UpdatedAt should advance")
}

func TestUnit_MCPServers_Delete(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("delete-me")
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	require.NoError(t, s.DeleteMCPServer(ctx, srv.ID))

	_, err := s.GetMCPServer(ctx, srv.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, libdb.ErrNotFound))
}

// ─── List & Pagination ─────────────────────────────────────────────────────────

func TestUnit_MCPServers_ListEmpty(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	items, err := s.ListMCPServers(ctx, nil, 100)
	require.NoError(t, err)
	require.Empty(t, items, "fresh DB should return empty list")
}

func TestUnit_MCPServers_List(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	servers := []*runtimetypes.MCPServer{
		newSSE("list-1"),
		newSSE("list-2"),
		newSSE("list-3"),
	}
	for _, srv := range servers {
		require.NoError(t, s.CreateMCPServer(ctx, srv))
	}

	items, err := s.ListMCPServers(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, items, 3)

	// Reverse-chronological order (newest first)
	require.Equal(t, servers[2].ID, items[0].ID)
	require.Equal(t, servers[1].ID, items[1].ID)
	require.Equal(t, servers[0].ID, items[2].ID)
}

func TestUnit_MCPServers_ListPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	var created []*runtimetypes.MCPServer
	for i := range 5 {
		srv := newSSE(fmt.Sprintf("pagination-mcp-%d", i))
		require.NoError(t, s.CreateMCPServer(ctx, srv))
		created = append(created, srv)
	}

	var received []*runtimetypes.MCPServer
	var cursor *time.Time
	const limit = 2

	page1, err := s.ListMCPServers(ctx, cursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	received = append(received, page1...)
	cursor = &page1[len(page1)-1].CreatedAt

	page2, err := s.ListMCPServers(ctx, cursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	received = append(received, page2...)
	cursor = &page2[len(page2)-1].CreatedAt

	page3, err := s.ListMCPServers(ctx, cursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	received = append(received, page3...)

	// Fourth page must be empty
	page4, err := s.ListMCPServers(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	require.Len(t, received, 5)
	// Newest first → created[4] is first received
	require.Equal(t, created[4].ID, received[0].ID)
	require.Equal(t, created[3].ID, received[1].ID)
	require.Equal(t, created[2].ID, received[2].ID)
	require.Equal(t, created[1].ID, received[3].ID)
	require.Equal(t, created[0].ID, received[4].ID)
}

// ─── Constraints ───────────────────────────────────────────────────────────────

func TestUnit_MCPServers_UniqueNameConstraint(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv1 := newSSE("unique-mcp-name")
	require.NoError(t, s.CreateMCPServer(ctx, srv1))

	srv2 := *srv1
	srv2.ID = uuid.New().String()
	srv2.URL = "http://other-mcp.example.com/sse"

	err := s.CreateMCPServer(ctx, &srv2)
	require.Error(t, err, "duplicate name must be rejected")
}

func TestUnit_MCPServers_DeleteAndRecreate(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("recreate-me")
	require.NoError(t, s.CreateMCPServer(ctx, srv))
	require.NoError(t, s.DeleteMCPServer(ctx, srv.ID))

	newSrv := *srv
	newSrv.ID = uuid.New().String()
	require.NoError(t, s.CreateMCPServer(ctx, &newSrv), "should allow recreating with same name after deletion")

	got, err := s.GetMCPServerByName(ctx, srv.Name)
	require.NoError(t, err)
	require.Equal(t, newSrv.ID, got.ID)
}

// ─── Not-found cases ───────────────────────────────────────────────────────────

func TestUnit_MCPServers_NotFoundCases(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	t.Run("get_by_id_not_found", func(t *testing.T) {
		_, err := s.GetMCPServer(ctx, uuid.New().String())
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("get_by_name_not_found", func(t *testing.T) {
		_, err := s.GetMCPServerByName(ctx, "non-existent-mcp")
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("update_non_existent", func(t *testing.T) {
		err := s.UpdateMCPServer(ctx, newSSE("ghost"))
		require.Error(t, err)
		require.True(t, errors.Is(err, libdb.ErrNotFound))
	})

	t.Run("delete_non_existent", func(t *testing.T) {
		err := s.DeleteMCPServer(ctx, uuid.New().String())
		require.Error(t, err)
	})
}

// ─── Concurrent updates ───────────────────────────────────────────────────────

func TestUnit_MCPServers_ConcurrentUpdates(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("concurrent-mcp")
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	updateURL := func(url string) {
		h, err := s.GetMCPServer(ctx, srv.ID)
		require.NoError(t, err)
		h.URL = url
		require.NoError(t, s.UpdateMCPServer(ctx, h))
	}

	var wg sync.WaitGroup
	urls := []string{
		"http://node1.example.com/sse",
		"http://node2.example.com/sse",
		"http://node3.example.com/sse",
	}
	for _, u := range urls {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			updateURL(url)
		}(u)
	}
	wg.Wait()

	final, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	require.Contains(t, urls, final.URL)
	require.True(t, final.UpdatedAt.After(srv.UpdatedAt))
}

// ─── Estimate count ───────────────────────────────────────────────────────────

func TestUnit_MCPServers_EstimateCount(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	for i := range 3 {
		require.NoError(t, s.CreateMCPServer(ctx, newSSE(fmt.Sprintf("count-%d", i))))
	}

	// EstimateMCPServerCount uses pg_class.reltuples on Postgres (may return -1
	// for freshly-created tables before ANALYZE runs) and COUNT(*) on SQLite.
	// We just verify the call completes without error.
	_, err := s.EstimateMCPServerCount(ctx)
	require.NoError(t, err)
}

// ─── headers_json / inject_params_json round-trips ───────────────────────────
// These tests specifically cover the fields that were silently absent from the
// SELECT queries in GetMCPServer and GetMCPServerByName before the fix, causing
// a runtime "sql: expected 12 destination arguments in Scan, not 14" error.
// Any future field added to MCPServer must be tested here.

func TestUnit_MCPServers_HeadersAndInjectParams_GetByID(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("inject-by-id-" + uuid.New().String()[:8])
	srv.Headers = map[string]string{"X-Tenant": "acme", "X-Version": "2"}
	srv.InjectParams = map[string]string{"tenant_id": "acme", "env": "production"}

	require.NoError(t, s.CreateMCPServer(ctx, srv))

	got, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"X-Tenant": "acme", "X-Version": "2"}, got.Headers)
	require.Equal(t, map[string]string{"tenant_id": "acme", "env": "production"}, got.InjectParams)
}

func TestUnit_MCPServers_HeadersAndInjectParams_GetByName(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	name := "inject-by-name-" + uuid.New().String()[:8]
	srv := newSSE(name)
	srv.Headers = map[string]string{"Authorization": "Bearer tok"}
	srv.InjectParams = map[string]string{"correlation_id": "trace-xyz"}

	require.NoError(t, s.CreateMCPServer(ctx, srv))

	got, err := s.GetMCPServerByName(ctx, name)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Authorization": "Bearer tok"}, got.Headers)
	require.Equal(t, map[string]string{"correlation_id": "trace-xyz"}, got.InjectParams)
}

func TestUnit_MCPServers_UpdateReplacesInjectParamsAndHeaders(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("inject-update-" + uuid.New().String()[:8])
	srv.Headers = map[string]string{"Old-Header": "1"}
	srv.InjectParams = map[string]string{"old_key": "old_val", "extra": "gone"}
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	srv.Headers = map[string]string{"New-Header": "2"}
	srv.InjectParams = map[string]string{"tenant_id": "new"}
	require.NoError(t, s.UpdateMCPServer(ctx, srv))

	got, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"New-Header": "2"}, got.Headers)
	require.Equal(t, map[string]string{"tenant_id": "new"}, got.InjectParams)
	_, hasExtra := got.InjectParams["extra"]
	require.False(t, hasExtra, "update must replace entire map, not merge")
}

func TestUnit_MCPServers_ListIncludesInjectParamsAndHeaders(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newSSE("inject-list-" + uuid.New().String()[:8])
	srv.Headers = map[string]string{"X-Key": "val"}
	srv.InjectParams = map[string]string{"key": "val"}
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	list, err := s.ListMCPServers(ctx, nil, 100)
	require.NoError(t, err)

	var found *runtimetypes.MCPServer
	for _, m := range list {
		if m.ID == srv.ID {
			found = m
			break
		}
	}
	require.NotNil(t, found, "created server must appear in list")
	require.Equal(t, map[string]string{"X-Key": "val"}, found.Headers)
	require.Equal(t, map[string]string{"key": "val"}, found.InjectParams)
}

func TestUnit_MCPServers_EmptyMapsRoundTrip(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	srv := newStdio("empty-maps-" + uuid.New().String()[:8])
	// No Headers or InjectParams — verify no crash and empty result.
	require.NoError(t, s.CreateMCPServer(ctx, srv))

	got, err := s.GetMCPServer(ctx, srv.ID)
	require.NoError(t, err)
	require.Empty(t, got.InjectParams)
	require.Empty(t, got.Headers)
}

// ─── RemoteTools inject_params_json round-trips ────────────────────────────────

func newTools(name string) *runtimetypes.RemoteTools {
	return &runtimetypes.RemoteTools{
		ID:          uuid.New().String(),
		Name:        name,
		EndpointURL: "https://api.example.com/tools",
		TimeoutMs:   5000,
	}
}

func TestUnit_RemoteTools_InjectParamsRoundTrip(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := newTools("tools-inject-" + uuid.New().String()[:8])
	tools.InjectParams = map[string]string{"tenant_id": "acme", "env": "prod"}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	got, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"tenant_id": "acme", "env": "prod"}, got.InjectParams)
}

func TestUnit_RemoteTools_UpdateInjectParamsReplaceAll(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := newTools("tools-upd-" + uuid.New().String()[:8])
	tools.InjectParams = map[string]string{"old": "value", "extra": "gone"}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	tools.InjectParams = map[string]string{"tenant_id": "new"}
	require.NoError(t, s.UpdateRemoteTools(ctx, tools))

	got, err := s.GetRemoteTools(ctx, tools.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"tenant_id": "new"}, got.InjectParams)
	_, hasExtra := got.InjectParams["extra"]
	require.False(t, hasExtra, "update must replace entire map")
}

func TestUnit_RemoteTools_ListIncludesInjectParams(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	tools := newTools("tools-list-" + uuid.New().String()[:8])
	tools.InjectParams = map[string]string{"correlation_id": "trace-abc"}
	require.NoError(t, s.CreateRemoteTools(ctx, tools))

	list, err := s.ListRemoteTools(ctx, nil, 100)
	require.NoError(t, err)

	var found *runtimetypes.RemoteTools
	for _, h := range list {
		if h.ID == tools.ID {
			found = h
			break
		}
	}
	require.NotNil(t, found)
	require.Equal(t, map[string]string{"correlation_id": "trace-abc"}, found.InjectParams)
}
