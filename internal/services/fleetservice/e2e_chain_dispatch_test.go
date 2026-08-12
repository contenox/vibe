package fleetservice

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/chainagents"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

const chainFixtureReply = "contenox chain unit fixture reply"

func buildContenoxBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/cmd/contenox")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build contenox: %v\n%s", err, out)
	}
	return binPath
}

func runContenoxCLI(t *testing.T, bin, home string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("contenox %v: %v\n%s", args, err, out)
	}
}

func writeChainAgentFixture(t *testing.T, contenoxDir string) string {
	t.Helper()
	chain := map[string]any{
		"id":          "agent-fleet-fixture",
		"description": "Deterministic no-model chain unit: one noop task whose print is the reply.",
		"tasks": []map[string]any{{
			"id":          "reply",
			"description": "Answer every prompt with the fixed fixture text.",
			"handler":     "noop",
			"print":       chainFixtureReply,
		}},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	path := filepath.Join(contenoxDir, "chain-agent-fleet-fixture.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

type recordingViewer struct {
	id string

	mu      sync.Mutex
	updates []libacp.SessionNotification
}

func (v *recordingViewer) ID() string { return v.id }

func (v *recordingViewer) Deliver(_ context.Context, n libacp.SessionNotification) error {
	// Deliver must not block: kernel contract.
	v.mu.Lock()
	v.updates = append(v.updates, n)
	v.mu.Unlock()
	return nil
}

func (v *recordingViewer) RequestPermission(_ context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	// Fail-closed: this fixture never triggers a gated tool, but deny is the safe default if that changes.
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
	}, nil
}

func (v *recordingViewer) messageText() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var sb strings.Builder
	for _, n := range v.updates {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// TestFleetE2E_ChainAgent_DiscoveredDispatchedAndAnswers: a chain file named
// by convention is discovered, dispatched, and streams its reply back to an
// attached viewer.
func TestFleetE2E_ChainAgent_DiscoveredDispatchedAndAnswers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chain-unit e2e: builds and boots the full contenox binary")
	}

	bin := buildContenoxBinary(t)
	home := t.TempDir()

	t.Setenv("HOME", home)
	for _, k := range []string{
		"CONTENOX_DEFAULT_MODEL", "CONTENOX_DEFAULT_PROVIDER",
		"CONTENOX_DEFAULT_ALT_MODEL", "CONTENOX_DEFAULT_ALT_PROVIDER",
		"CONTENOX_DEFAULT_MAX_TOKENS", "CONTENOX_DEFAULT_THINK",
		"CONTENOX_ACP_CHAIN_PATH",
	} {
		t.Setenv(k, "")
	}

	runContenoxCLI(t, bin, home, "config", "set", "default-model", "chain-unit-fixture-model")
	runContenoxCLI(t, bin, home, "config", "set", "update-check", "false")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir, "the CLI seeding run must have created the isolated state directory")
	chainPath := writeChainAgentFixture(t, contenoxDir)

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleet-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	agents := agentregistryservice.New(db)
	missions := missionservice.New(db)

	res, err := chainagents.Discover(ctx, agents, contenoxDir)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-fleet-fixture"}, res.Created,
		"naming the file chain-agent-*.json is the whole declaration")

	declared, err := agents.GetByName(ctx, "agent-fleet-fixture")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.AgentKindChain, declared.Kind)
	cfg, err := declared.ChainConfig()
	require.NoError(t, err)
	require.Equal(t, chainPath, cfg.Path)

	// Under `go test`, os.Executable() is the test binary, which serves no
	// ACP, so the kernel is pointed at the freshly built binary instead.
	stderr := &lockedBuffer{}
	instances := agentinstance.New(agents,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = instances.Close() })

	workDir := t.TempDir()
	svc := New(instances, agents, missions, nil, workDir, libtracker.NoopTracker{})

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "agent-fleet-fixture",
		Intent:         "answer through the chain unit",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "chain unit stderr:\n%s", stderr.String())
	require.NotEmpty(t, result.InstanceID)
	require.NotEmpty(t, result.SessionID)
	require.NotEmpty(t, result.MissionID, "every dispatch is a mission, chain units included")

	// Dispatch runs the intent as a detached first turn, so the attach may
	// land mid-turn; the journal replay covers whatever already streamed.
	viewer := &recordingViewer{id: "e2e-observer"}
	controller, err := instances.Attach(ctx, result.InstanceID, libacp.SessionID(result.SessionID), viewer)
	require.NoError(t, err)
	require.True(t, controller, "the first viewer of an unattended dispatched session becomes its controller")

	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), chainFixtureReply)
	}, 120*time.Second, 100*time.Millisecond,
		"chain unit never streamed its reply; got %q\nstderr:\n%s", viewer.messageText(), stderr.String())

	status, err := svc.Get(ctx, result.InstanceID)
	require.NoError(t, err)
	require.Equal(t, agentinstance.StateRunning, status.State)
	require.Equal(t, runtimetypes.AgentKindChain, status.Kind)
	require.Contains(t, status.SessionIDs, result.SessionID)

	require.NoError(t, svc.Stop(ctx, result.InstanceID))
	_, err = svc.Get(ctx, result.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
