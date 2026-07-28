package agenthost_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// buildStubAgent compiles the hermetic acp-stub-agent and returns its path.
func buildStubAgent(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/libacp/cmd/acp-stub-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build acp-stub-agent: %v\n%s", err, out)
	}
	return binPath
}

// TestHost_ExternalACPAgent_ConnectAndInitialize pins the host seam: Connect
// spawns a real agent and wires a live connection; Close tears it down cleanly.
func TestHost_ExternalACPAgent_ConnectAndInitialize(t *testing.T) {
	requireSandboxable(t)
	agentBin := buildStubAgent(t)

	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   agentBin,
		// Connect has no session cwd to default from (unlike DriveTurn).
		Cwd: t.TempDir(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// agenthost never assembles a harness itself; this is the no-op case.
	handle, err := host.Connect(ctx, libacp.UnimplementedClient{})
	require.NoError(t, err)
	require.NotNil(t, handle)
	require.NotNil(t, handle.Conn)
	defer handle.Close()

	resp, err := handle.Conn.Initialize(ctx, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientCapabilities: libacp.ClientCapabilities{
			FS: libacp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
		},
		ClientInfo: &libacp.Implementation{Name: "agenthost-test", Version: "test"},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.ProtocolVersion, resp.ProtocolVersion)
	require.NotNil(t, resp.AgentInfo)
	require.Equal(t, "acp-stub-agent", resp.AgentInfo.Name)

	require.NoError(t, handle.Close())

	select {
	case <-handle.Conn.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("ClientSideConnection did not report closed after Handle.Close")
	}
}

// TestHost_ExternalACPAgent_CloseIsIdempotent pins that calling Close twice
// returns the same result both times.
func TestHost_ExternalACPAgent_CloseIsIdempotent(t *testing.T) {
	requireSandboxable(t)
	agentBin := buildStubAgent(t)

	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   agentBin,
		Cwd:       t.TempDir(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	handle, err := host.Connect(ctx, libacp.UnimplementedClient{})
	require.NoError(t, err)

	err1 := handle.Close()
	err2 := handle.Close()
	require.NoError(t, err1)
	require.Equal(t, err1, err2)
}

func TestHost_ExternalACPAgent_Connect_RejectsNilHarness(t *testing.T) {
	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "irrelevant-not-actually-spawned",
	})

	_, err := host.Connect(context.Background(), nil)
	require.Error(t, err)
}

func TestHost_ExternalACPAgent_Connect_RejectsInvalidConfig(t *testing.T) {
	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		// Command deliberately omitted: invalid for stdio transport.
	})

	_, err := host.Connect(context.Background(), libacp.UnimplementedClient{})
	require.Error(t, err)
}

// TestHost_ExternalACPAgent_Connect_EndpointNotImplemented pins that endpoint
// transport errors immediately instead of hanging.
func TestHost_ExternalACPAgent_Connect_EndpointNotImplemented(t *testing.T) {
	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportEndpoint,
		URL:       "https://agent.example.com/acp",
	})

	_, err := host.Connect(context.Background(), libacp.UnimplementedClient{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not implemented")
}
