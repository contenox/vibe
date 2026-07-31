package agenthost_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/mcpserverservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/libacp"
	"github.com/contenox/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// This file covers MCP forwarding: an agent row's mcp_servers allowlist
// resolved against registered MCP servers and passed down in session/new.

const hostMcpEchoBinEnv = "ACP_MCP_ECHO_BIN"

// registerAgentWithMcp is registerAgent plus the MCP leg: it registers
// mcpRows and resolves allowlist into TurnRequest.McpServers.
func registerAgentWithMcp(t *testing.T, name, command string, mcpRows []*runtimetypes.MCPServer, allowlist []string) (context.Context, *runtimetypes.Agent, []libacp.McpServer) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "agenthost-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mcpSvc := mcpserverservice.New(db)
	for _, row := range mcpRows {
		require.NoError(t, mcpSvc.Create(ctx, row))
	}

	svc := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport:  runtimetypes.ExternalACPTransportStdio,
		Command:    command,
		McpServers: allowlist,
	}))
	require.NoError(t, svc.Create(ctx, agent))

	resolved, err := svc.GetByName(ctx, name)
	require.NoError(t, err)
	cfg, err := resolved.ExternalACPConfig()
	require.NoError(t, err)

	servers, err := agenthost.ResolveForwardedMcpServers(ctx, mcpSvc, cfg.McpServers)
	require.NoError(t, err)
	return ctx, resolved, servers
}

// TestHostE2E_Testy_McpPassDownThroughComposedPath pins that a forwarded MCP
// server spec is complete enough for testy to connect and use it.
func TestHostE2E_Testy_McpPassDownThroughComposedPath(t *testing.T) {
	requireSandboxable(t)
	testyBin := testyBinFromEnv(t)
	mcpBin := os.Getenv(hostMcpEchoBinEnv)
	if mcpBin == "" {
		t.Skipf("skipping: set %s to a built mcp-echo-server binary to run (see `make acp-client-e2e`)", hostMcpEchoBinEnv)
	}
	if _, err := os.Stat(mcpBin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", hostMcpEchoBinEnv, mcpBin, err)
	}

	ctx, agent, servers := registerAgentWithMcp(t, "testy-mcp", testyBin,
		[]*runtimetypes.MCPServer{{Name: "echo", Transport: "stdio", Command: mcpBin, ConnectTimeoutSeconds: 30}},
		[]string{"echo"})

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stderr acpexec.LockedBuffer
	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:        t.TempDir(),
		Prompt:     testyCommandPrompt(t, map[string]any{"command": "list_tools", "server": "echo"}),
		Stderr:     &stderr,
		KillGrace:  500 * time.Millisecond,
		McpServers: servers,
	})
	require.NoError(t, err, "testy stderr:\n%s", stderr.String())

	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason)
	require.Equal(t, []string{"echo"}, res.ForwardedMcpServers)
	require.Empty(t, res.DroppedMcpServers)

	reply := harness.MessageText()
	require.Contains(t, reply, "echo", "testy stderr:\n%s", stderr.String())
	require.Contains(t, reply, "Echoes back the input message", "testy stderr:\n%s", stderr.String())
}

// TestHost_DriveTurn_McpCapabilityFilterDropsUnsupported pins that an
// unsupported-transport server is withheld and reported.
func TestHost_DriveTurn_McpCapabilityFilterDropsUnsupported(t *testing.T) {
	requireSandboxable(t)
	stubBin := buildStubAgent(t)
	ctx, agent := registerAgent(t, "stub-mcp-filter", stubBin)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:    t.TempDir(),
		Prompt: []libacp.ContentBlock{libacp.NewTextContent("hello")},
		McpServers: []libacp.McpServer{
			{Name: "local-stdio", Command: "some-mcp-server"},
			{Name: "remote-http", Type: "http", URL: "https://mcp.example.com"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason)
	require.Equal(t, []string{"local-stdio"}, res.ForwardedMcpServers)
	require.Equal(t, []string{"remote-http"}, res.DroppedMcpServers)
}
