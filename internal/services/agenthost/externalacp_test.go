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

// buildStubAgent compiles libacp/cmd/acp-stub-agent — the hermetic,
// in-memory ACP Agent this repo already uses to exercise libacp's wire
// dispatch without any LLM backend — into t.TempDir() and returns its path.
// It gives this package's host test a real ACP agent subprocess to spawn and
// drive without depending on any external binary (contrast the
// ACP_TESTY_BIN-gated e2e tests in libacp/acpexec).
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

// TestHost_ExternalACPAgent_ConnectAndInitialize is the core assertion this
// package exists for: given an ExternalACPConfig pointing at a real ACP
// agent binary, ExternalACPAgent.Connect spawns it, wires a live
// ClientSideConnection to it over stdio, and a client-side "initialize" call
// through that connection succeeds against the real subprocess — proving
// the host seam (harness supplied by the caller, transport wired
// underneath) actually works end to end, not just in mocks. Close then
// tears the whole thing down cleanly.
func TestHost_ExternalACPAgent_ConnectAndInitialize(t *testing.T) {
	requireSandboxable(t)
	agentBin := buildStubAgent(t)

	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   agentBin,
		// The sandbox is the only spawn path and needs a workspace; Connect
		// (unlike DriveTurn) has no session cwd to default from, so it is set here.
		Cwd: t.TempDir(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// libacp.UnimplementedClient{} is the minimal/no-op harness the task
	// calls for: agenthost never assembles a harness itself, it only takes
	// whatever libacp.Client the caller passes.
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

// TestHost_ExternalACPAgent_CloseIsIdempotent drives a second, independent
// full connect/initialize/close cycle and asserts calling Close twice is
// safe and returns the same result both times.
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

// TestHost_ExternalACPAgent_Connect_EndpointNotImplemented locks in that the
// endpoint transport returns a clear, immediate error instead of hanging or
// silently no-op'ing — this task's scope stops at stdio; endpoint is a
// stubbed seam for later.
func TestHost_ExternalACPAgent_Connect_EndpointNotImplemented(t *testing.T) {
	host := agenthost.NewExternalACPAgent(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportEndpoint,
		URL:       "https://agent.example.com/acp",
	})

	_, err := host.Connect(context.Background(), libacp.UnimplementedClient{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not implemented")
}
