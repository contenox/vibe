package contenoxcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

const declWithSources = `---
name: researcher
description: Researches things
mcpServers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
remoteTools:
  billing:
    url: https://internal.example.com
    spec: https://internal.example.com/openapi.json
---

You research things.
`

func declaredToolsFixture(t *testing.T, body string) (context.Context, string, runtimetypes.Store, func()) {
	t.Helper()
	contenoxDir := t.TempDir()
	if body != "" {
		agentsDir := filepath.Join(contenoxDir, agentdecl.NativeSourceDir)
		require.NoError(t, os.MkdirAll(agentsDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "researcher.md"), []byte(body), 0o644))
	}
	ctx, svc, done := openServiceAt(t, filepath.Join(t.TempDir(), "agents.db"))
	_ = svc
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenDBAt(ctx, dbPath)
	require.NoError(t, err)
	return ctx, contenoxDir, runtimetypes.New(db.WithoutTransaction()), func() {
		_ = db.Close()
		done()
	}
}

func mcpNames(t *testing.T, ctx context.Context, store runtimetypes.Store) []string {
	t.Helper()
	page, err := store.ListMCPServers(ctx, nil, 100)
	require.NoError(t, err)
	out := make([]string, 0, len(page))
	for _, srv := range page {
		out = append(out, srv.Name)
	}
	return out
}

func remoteNames(t *testing.T, ctx context.Context, store runtimetypes.Store) []string {
	t.Helper()
	page, err := store.ListRemoteTools(ctx, nil, 100)
	require.NoError(t, err)
	out := make([]string, 0, len(page))
	for _, tool := range page {
		out = append(out, tool.Name)
	}
	return out
}

// The whole point in one test: a declaration brings its own tool sources, they
// are registered scoped to it, and deleting the declaration retires them.
func TestUnit_DeclaredToolSourcesRegisterAndRetire(t *testing.T) {
	ctx, contenoxDir, store, cleanup := declaredToolsFixture(t, declWithSources)
	defer cleanup()

	_, agents, agentsDone := openServiceAt(t, filepath.Join(t.TempDir(), "registry.db"))
	defer agentsDone()

	discoverChainAgents(ctx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{Store: store})

	wantMCP := runtimetypes.DeclaredToolName("researcher", "filesystem")
	wantRemote := runtimetypes.DeclaredToolName("researcher", "billing")
	require.Contains(t, mcpNames(t, ctx, store), wantMCP)
	require.Contains(t, remoteNames(t, ctx, store), wantRemote)

	srv, err := store.GetMCPServerByName(ctx, wantMCP)
	require.NoError(t, err)
	require.Equal(t, "stdio", srv.Transport)
	require.Equal(t, "npx", srv.Command)

	tool, err := store.GetRemoteToolsByName(ctx, wantRemote)
	require.NoError(t, err)
	require.Equal(t, "https://internal.example.com", tool.EndpointURL)
	require.Equal(t, "https://internal.example.com/openapi.json", tool.SpecURL)
	require.Equal(t, defaultDeclaredRemoteTimeoutMs, tool.TimeoutMs, "an unstated timeout matches `tools add`'s own default")

	// Delete the declaration; the next pass reconciles it away. No bookkeeping
	// recorded that it existed — the desired set simply no longer contains it.
	require.NoError(t, os.Remove(filepath.Join(contenoxDir, agentdecl.NativeSourceDir, "researcher.md")))
	discoverChainAgents(ctx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{Store: store})

	require.NotContains(t, mcpNames(t, ctx, store), wantMCP, "a deleted declaration retires the server it brought")
	require.NotContains(t, remoteNames(t, ctx, store), wantRemote)
}

// The operator's own registrations are a different owner and are never
// reconciled away, however many declarations come and go around them.
func TestUnit_DeclaredReconcileLeavesOperatorRegistrationsAlone(t *testing.T) {
	ctx, contenoxDir, store, cleanup := declaredToolsFixture(t, declWithSources)
	defer cleanup()

	require.NoError(t, store.UpsertMCPServerByName(ctx, &runtimetypes.MCPServer{
		Name: "github", Transport: "http", URL: "https://api.github.com/mcp", ConnectTimeoutSeconds: 30,
	}))
	require.NoError(t, store.CreateRemoteTools(ctx, &runtimetypes.RemoteTools{
		Name: "payments", EndpointURL: "https://pay.example.com", TimeoutMs: 5000,
	}))

	_, agents, agentsDone := openServiceAt(t, filepath.Join(t.TempDir(), "registry.db"))
	defer agentsDone()

	discoverChainAgents(ctx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{Store: store})
	require.Contains(t, mcpNames(t, ctx, store), "github")
	require.Contains(t, remoteNames(t, ctx, store), "payments")

	require.NoError(t, os.Remove(filepath.Join(contenoxDir, agentdecl.NativeSourceDir, "researcher.md")))
	discoverChainAgents(ctx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{Store: store})

	require.Contains(t, mcpNames(t, ctx, store), "github", "`contenox mcp add` registrations are the operator's")
	require.Contains(t, remoteNames(t, ctx, store), "payments")
}

// Re-running discovery must not accumulate duplicates or churn rows.
func TestUnit_DeclaredReconcileIsIdempotent(t *testing.T) {
	ctx, contenoxDir, store, cleanup := declaredToolsFixture(t, declWithSources)
	defer cleanup()

	_, agents, agentsDone := openServiceAt(t, filepath.Join(t.TempDir(), "registry.db"))
	defer agentsDone()

	for range 3 {
		discoverChainAgents(ctx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{Store: store})
	}
	require.Len(t, mcpNames(t, ctx, store), 1)
	require.Len(t, remoteNames(t, ctx, store), 1)
}

// Without a store the pass still reads declarations and writes chains; it just
// registers nothing. That is what `contenox agent list` does on a host with no
// engine, and it must not fail.
func TestUnit_DiscoveryWithoutAStoreRegistersNothing(t *testing.T) {
	ctx, contenoxDir, store, cleanup := declaredToolsFixture(t, declWithSources)
	defer cleanup()

	_, agents, agentsDone := openServiceAt(t, filepath.Join(t.TempDir(), "registry.db"))
	defer agentsDone()

	discoverChainAgents(ctx, agents, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{})

	require.Empty(t, mcpNames(t, ctx, store))
	_, err := agents.GetByName(ctx, "researcher")
	require.NoError(t, err, "the agent still registers")
}
