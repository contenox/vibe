package contenoxcli

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// TestUnit_RedirectBeamLogsToFile asserts a WARN goes to the file and never to stderr, which beam is about to take over as the screen.
func TestUnit_RedirectBeamLogsToFile(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scratch.db")

	logPath, _, closeLog, err := redirectBeamLogsToFile(dbPath)
	require.NoError(t, err)
	t.Cleanup(closeLog)

	require.Equal(t, filepath.Join(dir, beamLogFileName), logPath,
		"the log lives beside the database beam actually opened, so a --db scratch run keeps its warnings to itself")

	slog.Warn("beam-test-warning", "detail", "visible")
	slog.Info("beam-test-info")
	closeLog()

	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	body := string(raw)
	require.Contains(t, body, "beam-test-warning", "WARN must reach the file")
	require.Contains(t, body, "visible")
	require.NotContains(t, body, "beam-test-info", "the level is WARN+: this is a log to open when something looked wrong, not a trace")
}

// TestUnit_RedirectBeamLogsToFile_AppendsAcrossRuns asserts the log file accumulates warnings across relaunches rather than truncating.
func TestUnit_RedirectBeamLogsToFile_AppendsAcrossRuns(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dbPath := filepath.Join(t.TempDir(), "scratch.db")
	for _, msg := range []string{"first-run", "second-run"} {
		logPath, _, closeLog, err := redirectBeamLogsToFile(dbPath)
		require.NoError(t, err)
		slog.Warn(msg)
		closeLog()
		require.FileExists(t, logPath)
	}

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), beamLogFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), "first-run")
	require.Contains(t, string(raw), "second-run")
}

// TestUnit_BeamCmd_DefaultsToItsOwnPolicyAndChain pins beam's defaults: its
// own HITL preset (an attended-session envelope, not the shared acp/default
// ones) and its own chain (not `contenox acp`'s), each independently
// overridable via its own env var — editors keep using acp_cmd's defaults.
func TestUnit_BeamCmd_DefaultsToItsOwnPolicyAndChain(t *testing.T) {
	require.Equal(t, "hitl-policy-beam.json", beamHITLPolicy)
	require.Equal(t, "chain-agent-beam.json", beamChainFile)
	require.Equal(t, "CONTENOX_BEAM_CHAIN_PATH", beamChainEnv)
	require.NotEqual(t, beamChainEnv, "CONTENOX_ACP_CHAIN_PATH", "beam must not share acp_cmd's chain override var")
}

// TestUnit_RedirectBeamLogsToFile_ReportsAnUnwritableTarget asserts an unwritable log target returns an error rather than installing a broken handler.
func TestUnit_RedirectBeamLogsToFile_ReportsAnUnwritableTarget(t *testing.T) {
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) })

	dbPath := filepath.Join(t.TempDir(), "no-such-dir", "scratch.db")
	logPath, _, closeLog, err := redirectBeamLogsToFile(dbPath)
	require.Error(t, err)
	require.Empty(t, logPath)
	require.Nil(t, closeLog)
	require.True(t, strings.Contains(err.Error(), beamLogFileName), "the error must name the path it failed on: %v", err)
	require.Same(t, restore, slog.Default(), "a failed redirect must leave logging exactly where it was")
}

// beamWiringChain is a valid chain file for the loader's fail-closed
// validation; no turn is ever run against it here.
const beamWiringChain = `{"id":"beam-wiring-test","tasks":[{"id":"noop"}]}`

// beamWiringOpts is beam's posture reduced to what the wiring under test
// depends on: an approval gate, the one hitlservice.Service beam injects, and
// no backend cycle so the engine builds without a live provider.
func beamWiringOpts(t *testing.T, dir string, hitl hitlservice.Service) chatOpts {
	t.Helper()
	return chatOpts{
		ContenoxDir:               dir,
		EffectiveTracker:          libtracker.NoopTracker{},
		EffectiveHITL:             true,
		EffectiveHITLService:      hitl,
		EffectiveSkipBackendCycle: true,
		EffectiveDefaultModel:     defaultModel,
		EffectiveDefaultProvider:  "ollama",
	}
}

// parkBeamAsk writes the state a park leaves behind: an approval still pending
// against its session, and the checkpoint the run suspended into. Both halves
// matter — the checkpoint is what makes hitlservice refuse a verdict from a
// process that cannot resume the run, which is how this test can tell one
// service from two.
func parkBeamAsk(t *testing.T, store runtimetypes.Store, askID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID:          askID,
		ToolsName:   "local_shell",
		ToolName:    "exec",
		ArgsSummary: "find home/naro -name go.mod",
		PolicyName:  beamHITLPolicy,
		OnTimeout:   "deny",
		State:       runtimetypes.HITLApprovalPending,
		SessionID:   sessionID,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}))
	require.NoError(t, store.CreateChainCheckpoint(ctx, &runtimetypes.ChainCheckpoint{
		ID:        askID,
		Payload:   json.RawMessage(`{"parked":true}`),
		SessionID: sessionID,
	}))
}

// internalSessionID resolves the contenox session id behind an ACP session
// name — the id a durable ask is recorded against.
func internalSessionID(t *testing.T, db libdb.DBManager, workspaceID string, sid libacp.SessionID) string {
	t.Helper()
	roster, err := sessionservice.New(db, workspaceID, libtracker.NoopTracker{}).List(context.Background(), acpSessionIdentity)
	require.NoError(t, err)
	for _, s := range roster {
		if s.Name == string(sid) {
			return s.ID
		}
	}
	t.Fatalf("session %q is not in the roster", sid)
	return ""
}

// TestSystem_Beam_ReoffersAParkedApprovalOnAttach is the wiring assertion the
// incident asks for, and it is deliberately not a test of the re-offer: that
// one already existed, already passed, and is exactly what did not save us.
// What is asserted here is that the seam is FILLED on beam — that the deps
// beam's own constructor produces carry the ask inbox all the way into an ACP
// transport, so an approval a run parked on comes back to a client that
// attaches.
//
// It runs the real construction path: the real BuildEngine, the real
// beamBridgeDeps, the real enginebridge loopback. Only the parked state is
// staged, because producing it through a turn means waiting out
// localtools.ApprovalParkWindow.
//
// The verdict is asserted on the durable row, which is also the proof that
// this process holds ONE hitlservice.Service. hitlservice refuses a verdict
// for a checkpointed run from an instance carrying no resume hook, so the row
// only reaches approved if the service beam injected is the same one
// BuildEngine registered the hook on. A sibling would leave it pending.
func TestSystem_Beam_ReoffersAParkedApprovalOnAttach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	workspace := t.TempDir()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(dir, "beam.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	chainPath := filepath.Join(dir, "chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(beamWiringChain), 0o600))
	t.Setenv(beamChainEnv, chainPath)
	chains, err := acpsvc.LoadChainRegistryFrom(beamChainFile, beamChainEnv)
	require.NoError(t, err)

	store := runtimetypes.New(db.WithoutTransaction())
	hitl := newHITLService(dir, store, libtracker.NoopTracker{}, beamHITLPolicy)
	opts := beamWiringOpts(t, dir, hitl)

	engine, err := BuildEngine(ctx, db, opts)
	require.NoError(t, err)
	defer engine.Stop()

	deps := beamBridgeDeps(opts, enginebridge.Deps{
		Engine:        engine,
		DB:            db,
		ChainRegistry: chains,
		SessionRouter: acpsvc.NewSessionRouter(),
	})
	seam := deps.Asks
	require.True(t, seam != nil,
		"beam's bridge deps must carry the durable ask inbox; a typed nil is not a nil interface, so this is written out")

	bridge, err := enginebridge.New(ctx, deps)
	require.NoError(t, err)
	defer func() { require.NoError(t, bridge.Close()) }()

	_, err = bridge.Initialize(ctx)
	require.NoError(t, err)
	resp, err := bridge.NewSession(ctx, libacp.NewSessionRequest{Cwd: workspace, McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	askID := "ask-parked-beam"
	parkBeamAsk(t, store, askID, internalSessionID(t, db, deps.WorkspaceID, resp.SessionID))

	_, err = bridge.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  resp.SessionID,
		Cwd:        workspace,
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)

	deadline := time.After(30 * time.Second)
	for card := (enginebridge.PermissionRequested{}); card.ToolCallID != askID; {
		select {
		case ev, ok := <-bridge.Events():
			require.True(t, ok, "the bridge closed before the parked approval was re-offered")
			if requested, isCard := ev.(enginebridge.PermissionRequested); isCard {
				card = requested
				require.Equal(t, resp.SessionID, card.SessionID)
				require.Contains(t, card.Title, "find home/naro",
					"the row's args summary is what says which call is being gated")
				card.Resolve(true)
			}
		case <-deadline:
			t.Fatal("attaching to a session parked on an approval showed no card")
		}
	}

	require.Eventually(t, func() bool {
		row, getErr := store.GetHITLApproval(context.Background(), askID)
		return getErr == nil && row.State == runtimetypes.HITLApprovalApproved
	}, 30*time.Second, 20*time.Millisecond,
		"the verdict must land on the durable row through the one service the resume hook is registered on")
}
