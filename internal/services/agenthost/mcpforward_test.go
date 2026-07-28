package agenthost_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/internal/services/mcpserverservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func TestUnit_McpServerForACP_StdioAndHttpShapes(t *testing.T) {
	stdio, err := agenthost.McpServerForACP(&runtimetypes.MCPServer{
		Name: "fs", Transport: "stdio", Command: "mcp-fs", Args: []string{"--root", "/tmp"},
	})
	require.NoError(t, err)
	require.Equal(t, "fs", stdio.Name)
	require.Equal(t, "mcp-fs", stdio.Command)
	require.Equal(t, []string{"--root", "/tmp"}, stdio.Args)
	require.Empty(t, stdio.Type)

	http, err := agenthost.McpServerForACP(&runtimetypes.MCPServer{
		Name: "remote", Transport: "http", URL: "https://mcp.example.com",
		Headers: map[string]string{"X-B": "2", "X-A": "1"},
	})
	require.NoError(t, err)
	require.Equal(t, "http", http.Type)
	require.Equal(t, "https://mcp.example.com", http.URL)
	// Deterministic header order (sorted by name), not map order.
	require.Len(t, http.Headers, 2)
	require.Equal(t, "X-A", http.Headers[0].Name)
	require.Equal(t, "X-B", http.Headers[1].Name)
}

// TestUnit_McpServerForACP_NeverForwardsAuthSynthesis pins that contenox-side
// auth machinery never leaks into the payload handed to a foreign agent.
func TestUnit_McpServerForACP_NeverForwardsAuthSynthesis(t *testing.T) {
	const secret = "super-secret-token-do-not-forward"
	srv, err := agenthost.McpServerForACP(&runtimetypes.MCPServer{
		Name: "guarded", Transport: "sse", URL: "https://mcp.example.com/sse",
		AuthType: "bearer", AuthToken: secret, AuthEnvKey: "GUARDED_TOKEN",
		OAuthClientID: "oauth-client", OAuthClientSecretEnv: "OAUTH_SECRET",
		InjectParams: map[string]string{"api_key": secret},
	})
	require.NoError(t, err)

	wire, err := json.Marshal(srv)
	require.NoError(t, err)
	require.NotContains(t, string(wire), secret)
	require.NotContains(t, string(wire), "GUARDED_TOKEN")
	require.NotContains(t, string(wire), "oauth-client")
}

func TestUnit_McpServerForACP_RejectsBrokenRows(t *testing.T) {
	_, err := agenthost.McpServerForACP(&runtimetypes.MCPServer{Name: "no-cmd", Transport: "stdio"})
	require.Error(t, err)

	_, err = agenthost.McpServerForACP(&runtimetypes.MCPServer{Name: "no-url", Transport: "http"})
	require.Error(t, err)

	_, err = agenthost.McpServerForACP(&runtimetypes.MCPServer{Name: "weird", Transport: "carrier-pigeon", Command: "x"})
	require.Error(t, err)
}

// TestUnit_ResolveForwardedMcpServers_MissingNameIsLoud pins that an
// unresolvable allowlist name fails loudly instead of being skipped.
func TestUnit_ResolveForwardedMcpServers_MissingNameIsLoud(t *testing.T) {
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "mcpforward.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := mcpserverservice.New(db)

	require.NoError(t, svc.Create(ctx, &runtimetypes.MCPServer{
		Name: "present", Transport: "stdio", Command: "mcp-present", ConnectTimeoutSeconds: 30,
	}))

	servers, err := agenthost.ResolveForwardedMcpServers(ctx, svc, []string{"present"})
	require.NoError(t, err)
	require.Len(t, servers, 1)
	require.Equal(t, "present", servers[0].Name)

	_, err = agenthost.ResolveForwardedMcpServers(ctx, svc, []string{"present", "ghost"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ghost")
}
