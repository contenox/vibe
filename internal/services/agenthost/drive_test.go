package agenthost_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// registerAgent creates an agents row via the registry service and resolves it back.
func registerAgent(t *testing.T, name, command string, args ...string) (context.Context, *runtimetypes.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "agenthost-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   command,
		Args:      args,
	}))
	require.NoError(t, svc.Create(ctx, agent))

	resolved, err := svc.GetByName(ctx, name)
	require.NoError(t, err)
	return ctx, resolved
}

// TestHost_DriveTurn_RegistryToStubRoundTrip drives registry → host → live
// ACP session → answer, with nothing mocked.
func TestHost_DriveTurn_RegistryToStubRoundTrip(t *testing.T) {
	requireSandboxable(t)
	stubBin := buildStubAgent(t)
	ctx, agent := registerAgent(t, "stub-roundtrip", stubBin)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stderr acpexec.LockedBuffer
	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:    t.TempDir(),
		Prompt: []libacp.ContentBlock{libacp.NewTextContent("hello from the composed host")},
		Stderr: &stderr,
	})
	require.NoError(t, err, "stub stderr:\n%s", stderr.String())

	require.Equal(t, libacp.ProtocolVersion, res.Initialize.ProtocolVersion)
	require.NotNil(t, res.Initialize.AgentInfo)
	require.Equal(t, "acp-stub-agent", res.Initialize.AgentInfo.Name)
	require.NotEmpty(t, res.SessionID)
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason)

	// The plain-prompt path acks with exactly one message chunk.
	require.Equal(t, "ack", harness.MessageText())
	tracker := &libacp.TurnTracker{}
	for _, n := range harness.Updates() {
		tracker.Observe(n)
	}
	require.NoError(t, tracker.Err(res.StopReason))
}

// TestHost_DriveTurn_StreamingUpdatesReachHarness pins that the whole
// notification stream reaches the harness in wire order.
func TestHost_DriveTurn_StreamingUpdatesReachHarness(t *testing.T) {
	requireSandboxable(t)
	stubBin := buildStubAgent(t)
	ctx, agent := registerAgent(t, "stub-streaming", stubBin)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:    t.TempDir(),
		Prompt: []libacp.ContentBlock{libacp.NewTextContent(`{"command":"run_scenario","scenario":"session_updates"}`)},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason)

	require.Equal(t, "running scenario...done", harness.MessageText())

	var kinds []libacp.SessionUpdateKind
	for _, n := range harness.Updates() {
		require.Equal(t, res.SessionID, n.SessionID)
		kinds = append(kinds, n.Update.SessionUpdate)
	}
	require.Equal(t, []libacp.SessionUpdateKind{
		libacp.SessionUpdateAgentMessageChunk,
		libacp.SessionUpdateToolCall,
		libacp.SessionUpdateToolCallUpdate,
		libacp.SessionUpdateAgentMessageChunk,
	}, kinds)
}

// TestHost_DriveTurn_RejectsNonExternalKind pins that DriveTurn refuses a
// non-external_acp row instead of attempting to spawn.
func TestHost_DriveTurn_RejectsNonExternalKind(t *testing.T) {
	agent := &runtimetypes.Agent{Name: "future-chain", Kind: runtimetypes.AgentKindChain}

	_, err := agenthost.DriveTurn(context.Background(), agent, &agenthost.RecordingHarness{}, agenthost.TurnRequest{
		Cwd:    t.TempDir(),
		Prompt: []libacp.ContentBlock{libacp.NewTextContent("hi")},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "chain")
}

// TestHost_DriveTurn_RequiresCwdAndPrompt pins the required TurnRequest fields.
func TestHost_DriveTurn_RequiresCwdAndPrompt(t *testing.T) {
	agent := &runtimetypes.Agent{Name: "unused"}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "irrelevant-not-actually-spawned",
	}))

	_, err := agenthost.DriveTurn(context.Background(), agent, &agenthost.RecordingHarness{}, agenthost.TurnRequest{
		Prompt: []libacp.ContentBlock{libacp.NewTextContent("hi")},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Cwd")

	_, err = agenthost.DriveTurn(context.Background(), agent, &agenthost.RecordingHarness{}, agenthost.TurnRequest{
		Cwd: t.TempDir(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Prompt")
}

// TestUnit_DenyingHarness_SelectsRejectOptionAndRecords pins the reject_once
// > reject_always > cancelled order and that every ask is recorded.
func TestUnit_DenyingHarness_SelectsRejectOptionAndRecords(t *testing.T) {
	h := &agenthost.DenyingHarness{}

	resp, err := h.RequestPermission(context.Background(), libacp.RequestPermissionRequest{
		ToolCall: libacp.PermissionToolCall{ToolCallID: "tc-1", Title: "Write TODO.md"},
		Options: []libacp.PermissionOption{
			{OptionID: "ok", Name: "Allow", Kind: libacp.PermissionAllowOnce},
			{OptionID: "no-always", Name: "Reject always", Kind: libacp.PermissionRejectAlways},
			{OptionID: "no", Name: "Reject", Kind: libacp.PermissionRejectOnce},
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.PermissionOutcomeSelected, resp.Outcome.Outcome)
	require.Equal(t, "no", resp.Outcome.OptionID, "reject_once must win over reject_always")

	resp, err = h.RequestPermission(context.Background(), libacp.RequestPermissionRequest{
		ToolCall: libacp.PermissionToolCall{ToolCallID: "tc-2"},
		Options: []libacp.PermissionOption{
			{OptionID: "no-always", Name: "Reject always", Kind: libacp.PermissionRejectAlways},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "no-always", resp.Outcome.OptionID)

	resp, err = h.RequestPermission(context.Background(), libacp.RequestPermissionRequest{
		ToolCall: libacp.PermissionToolCall{ToolCallID: "tc-3", Title: "Run rm -rf"},
		Options: []libacp.PermissionOption{
			{OptionID: "ok", Name: "Allow", Kind: libacp.PermissionAllowOnce},
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.PermissionOutcomeCancelled, resp.Outcome.Outcome, "no reject option offered → cancelled")

	require.Equal(t, []string{"Write TODO.md", "tc-2", "Run rm -rf"}, h.Denied(),
		"every ask recorded, titled by ToolCall.Title with the id as fallback")
}
