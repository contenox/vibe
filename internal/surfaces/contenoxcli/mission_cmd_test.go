package contenoxcli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// ─── fire: the --wait requirement ───────────────────────────────────────────

// TestUnit_MissionFireRequiresWait pins the honest constraint the help text
// documents: a fired unit is a child of this process, so fire-and-detach from
// a one-shot CLI is refused with a teaching error BEFORE anything is opened.
func TestUnit_MissionFireRequiresWait(t *testing.T) {
	require.NoError(t, missionFireCmd.Flags().Set("wait", "false"))
	err := runMissionFire(missionFireCmd, []string{"some-agent", "do", "the", "thing"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--wait", "the refusal names the missing flag")
	require.Contains(t, err.Error(), "child subprocess", "the refusal teaches WHY --wait is required")
}

// ─── the wait/terminal loop ─────────────────────────────────────────────────

func setupMissionStore(t *testing.T) (context.Context, missionservice.Service) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "missions.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, missionservice.New(db)
}

func createOpenMission(t *testing.T, ctx context.Context, missions missionservice.Service) *missionservice.Mission {
	t.Helper()
	m := &missionservice.Mission{Intent: "wait-loop fixture", AgentName: "fixture", HITLPolicyName: "hitl-policy-default.json"}
	require.NoError(t, missions.Create(ctx, m))
	return m
}

func TestUnit_WaitForTerminalMission_ReturnsOnFinish(t *testing.T) {
	ctx, missions := setupMissionStore(t)
	m := createOpenMission(t, ctx, missions)

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = missions.Finish(ctx, m.ID, missionservice.StatusLanded, "fixture done")
	}()

	got, err := waitForTerminalMission(ctx, missions, m.ID, 10*time.Millisecond, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusLanded, got.Status)
	require.Equal(t, "fixture done", got.StatusReason)
}

func TestUnit_WaitForTerminalMission_TimesOutWhileOpen(t *testing.T) {
	ctx, missions := setupMissionStore(t)
	m := createOpenMission(t, ctx, missions)

	_, err := waitForTerminalMission(ctx, missions, m.ID, 10*time.Millisecond, 60*time.Millisecond)
	require.ErrorIs(t, err, errMissionWaitTimeout)
}

func TestUnit_WaitForTerminalMission_HonorsCancel(t *testing.T) {
	ctx, missions := setupMissionStore(t)
	m := createOpenMission(t, ctx, missions)

	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := waitForTerminalMission(cctx, missions, m.ID, 10*time.Millisecond, 5*time.Second)
	require.ErrorIs(t, err, context.Canceled)
}

// TestUnit_WaitForTerminalMission_AlreadyTerminal pins that a mission that
// finished before the wait started returns immediately, not after a tick.
func TestUnit_WaitForTerminalMission_AlreadyTerminal(t *testing.T) {
	ctx, missions := setupMissionStore(t)
	m := createOpenMission(t, ctx, missions)
	_, err := missions.Finish(ctx, m.ID, missionservice.StatusStuck, "hit a wall")
	require.NoError(t, err)

	start := time.Now()
	got, werr := waitForTerminalMission(ctx, missions, m.ID, time.Hour, time.Hour)
	require.NoError(t, werr)
	require.Equal(t, missionservice.StatusStuck, got.Status)
	require.Less(t, time.Since(start), 10*time.Second, "an already-terminal mission must not wait a tick")
}

// ─── outcome + exit-code surface ────────────────────────────────────────────

func TestUnit_MissionOutcomeLine(t *testing.T) {
	landed := &missionservice.Mission{ID: "m-1", Status: missionservice.StatusLanded}
	require.Equal(t, "Mission m-1 finished: landed", missionOutcomeLine(landed))

	stuck := &missionservice.Mission{ID: "m-2", Status: missionservice.StatusStuck, StatusReason: "compute spent"}
	require.Equal(t, "Mission m-2 finished: stuck — compute spent", missionOutcomeLine(stuck))
}

// ─── rendering ──────────────────────────────────────────────────────────────

func TestUnit_RenderMissionTable(t *testing.T) {
	now := time.Now().UTC()
	var empty bytes.Buffer
	require.NoError(t, renderMissionTable(&empty, nil, now))
	require.Contains(t, empty.String(), "No missions recorded", "an empty table teaches how to fire one")
	require.Contains(t, empty.String(), "mission fire")

	var buf bytes.Buffer
	require.NoError(t, renderMissionTable(&buf, []*missionservice.Mission{
		{ID: "m-1", AgentName: "agent-a", HITLPolicyName: "hitl-policy-default.json", Status: missionservice.StatusOpen, CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "m-2", AgentName: "agent-b", HITLPolicyName: "hitl-policy-strict.json", Status: missionservice.StatusLanded, CreatedAt: now.Add(-2 * 24 * time.Hour)},
	}, now))
	out := buf.String()
	for _, want := range []string{"ID", "AGENT", "ENVELOPE", "STATUS", "AGE", "m-1", "agent-a", "hitl-policy-default.json", "open", "3m", "m-2", "landed", "2d"} {
		require.Contains(t, out, want)
	}
}

func TestUnit_RenderMissionShow_SurfacesVerificationWarning(t *testing.T) {
	now := time.Now().UTC()
	m := &missionservice.Mission{
		ID: "m-3", Intent: "ship it", AgentName: "agent-a",
		HITLPolicyName: "hitl-policy-default.json",
		Status:         missionservice.StatusOpen,
		CreatedAt:      now.Add(-time.Hour), UpdatedAt: now,
	}
	reports := []*missionservice.Report{
		{
			Kind:    missionservice.ReportKindProgress,
			Summary: "claims a file that is not there",
			Detail:  "did the work\n\nclaimed artifacts not found: \"/tmp/nope.txt\" — the unit's result named artifacts that do not exist at their claimed paths.",
			Refs:    []string{"/tmp/nope.txt"},
		},
	}
	var buf bytes.Buffer
	renderMissionShow(&buf, m, reports, now)
	out := buf.String()
	require.Contains(t, out, "ship it")
	require.Contains(t, out, "refs: /tmp/nope.txt")
	require.Contains(t, out, verificationWarningLead, "the verification-gate warning is visible on show")
	require.NotContains(t, out, "did the work", "show prints summaries, not full detail")
}

func TestUnit_RenderMissionReports_FullDetailAndHandover(t *testing.T) {
	m := &missionservice.Mission{ID: "m-4", Intent: "hand over", Status: missionservice.StatusLanded}
	reports := []*missionservice.Report{
		// ListReports order: newest first. Rendering must flip to oldest first.
		{Kind: missionservice.ReportKindResult, Summary: "second", Detail: "the deep detail",
			Handover: &missionservice.Handover{Outcome: "done", Artifacts: []string{"a.txt"}, HandoverForNext: "pick up here", Caveats: "unverified"}},
		{Kind: missionservice.ReportKindProgress, Summary: "first"},
	}
	var buf bytes.Buffer
	renderMissionReports(&buf, m, reports)
	out := buf.String()
	require.Less(t, strings.Index(out, "first"), strings.Index(out, "second"), "reports read chronologically, oldest first")
	for _, want := range []string{"the deep detail", "Outcome:", "done", "a.txt", "pick up here", "unverified"} {
		require.Contains(t, out, want)
	}
}

func TestUnit_VerificationWarningLine(t *testing.T) {
	require.Equal(t, "", verificationWarningLine("plain detail, nothing downgraded"))
	got := verificationWarningLine("work happened\nclaimed artifacts not found: \"x\" — downgraded.\ntrailing")
	require.True(t, strings.HasPrefix(got, verificationWarningLead), "the extracted line starts with the lead, got %q", got)
}

func TestUnit_FormatMissionAge(t *testing.T) {
	now := time.Now()
	require.Equal(t, "45s", formatMissionAge(now, now.Add(-45*time.Second)))
	require.Equal(t, "12m", formatMissionAge(now, now.Add(-12*time.Minute)))
	require.Equal(t, "3h", formatMissionAge(now, now.Add(-3*time.Hour)))
	require.Equal(t, "2d", formatMissionAge(now, now.Add(-49*time.Hour)))
	require.Equal(t, "0s", formatMissionAge(now, now.Add(time.Minute)), "a future timestamp clamps to zero, never negative")
}

// ─── fire --wait end to end, against the in-process fleet ───────────────────

// fireFinisherChain is the deterministic, model-free chain the dispatched unit
// runs: file a result report through its granted mission tool, then bring the
// mission to rest with mission_finish — the terminal status `mission fire
// --wait` blocks on. No model, no network.
const fireFinisherChain = `{
  "id": "fire-finisher-chain",
  "description": "mission fire e2e: file a result report, then finish landed.",
  "tasks": [
    {
      "id": "report",
      "handler": "tools",
      "tools": {"name": "mission", "tool_name": "mission_report", "args": {"kind": "result", "summary": "fire e2e unit done"}},
      "transition": {"branches": [{"operator": "default", "goto": "finish"}]}
    },
    {
      "id": "finish",
      "handler": "tools",
      "tools": {"name": "mission", "tool_name": "mission_finish", "args": {"status": "landed", "reason": "fire e2e complete"}},
      "transition": {"branches": [{"operator": "default", "goto": "end"}]}
    }
  ]
}`

const fireAgentName = "fire-finisher"

// TestSystem_MissionFireWaitLandsInProcess is the acceptance for the CLI's
// blocking dispatch: `contenox mission fire <agent> <intent> --wait` embeds the
// fleet in THIS process, dispatches a real child subprocess bound to a
// deterministic finisher chain, waits for the terminal status, prints the
// outcome, and exits 0 — with the mission and its report durable in the shared
// db. Hermetic like the acp in-process e2e (isolated HOME, no serve, no model
// resolution, no network); see acp_mission_inproc_e2e_test.go for the shape.
func TestSystem_MissionFireWaitLandsInProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mission fire system e2e: builds contenox and spawns a real child subprocess")
	}

	bin := fwdBuildBin(t)

	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	workspaceDir := filepath.Join(root, "workspace")
	dataDir := filepath.Join(workspaceDir, ".contenox")
	dbPath := filepath.Join(homeDir, ".contenox", "local.db")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	baseEnv := append(os.Environ(),
		"HOME="+homeDir,
		// The CHILD unit builds an engine; a fake model keeps any accidental
		// resolution failing loudly while the model-free chain never touches it.
		"CONTENOX_DEFAULT_MODEL=fire-e2e-fake-model",
		"CONTENOX_DEFAULT_PROVIDER=ollama",
		"CONTENOX_SERVER_URL=",
		"CONTENOX_ACP_CHAIN_PATH=",
	)

	fwdRunCLI(t, bin, baseEnv, "--data-dir", dataDir, "--db", dbPath, "init", "--force")

	// The unit's chain lives OUTSIDE .contenox (control-plane isolation), read
	// via the declared agent's env.
	chainsDir := filepath.Join(root, "chains")
	require.NoError(t, os.MkdirAll(chainsDir, 0o755))
	chainPath := filepath.Join(chainsDir, "fire-finisher.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(fireFinisherChain), 0o644))

	// Declare the fired agent: an external `contenox acp --auto` unit bound to
	// the finisher chain (the chain env also marks it a dispatched unit, so it
	// hosts no fleet of its own).
	ctx := context.Background()
	seedDB, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	agent := &runtimetypes.Agent{Name: fireAgentName, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agentregistryservice.New(seedDB).Create(ctx, agent))
	require.NoError(t, clikv.WriteConfig(ctx, runtimetypes.New(seedDB.WithoutTransaction()), "", "update-check", "false"))
	require.NoError(t, seedDB.Close())

	// ── Fire and wait ────────────────────────────────────────────────────────
	intent := "run the fire e2e and land"
	cmd := exec.Command(bin, "--data-dir", dataDir, "--db", dbPath,
		"mission", "fire", fireAgentName, intent,
		"--policy", "hitl-policy-default.json", "--wait", "--timeout", "120s")
	cmd.Env = baseEnv
	cmd.Dir = workspaceDir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "mission fire --wait must exit 0 on a landed mission\noutput:\n%s", out)
	require.Contains(t, string(out), "Mission fired at agent")
	require.Contains(t, string(out), "finished: landed", "the terminal outcome line names the landed status")
	require.Contains(t, string(out), "fire e2e unit done", "the unit's result report summary is printed")

	// ── The mission and its report are durable facts in the shared db ────────
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	missions := missionservice.New(db)
	ms, err := missions.List(ctx, nil, 100)
	require.NoError(t, err)
	var fired *missionservice.Mission
	for _, m := range ms {
		if m.AgentName == fireAgentName && m.Intent == intent {
			fired = m
			break
		}
	}
	require.NotNil(t, fired, "the fired mission is a durable record")
	require.Equal(t, missionservice.StatusLanded, fired.Status)
	require.Empty(t, fired.ParentSessionID, "an operator-fired mission records no parent session")
	reports, err := missions.ListReports(ctx, fired.ID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, reports, "the unit's result report is durable")

	// ── The read verbs see it ────────────────────────────────────────────────
	listCmd := exec.Command(bin, "--data-dir", dataDir, "--db", dbPath, "mission", "list")
	listCmd.Env = baseEnv
	listOut, err := listCmd.CombinedOutput()
	require.NoErrorf(t, err, "mission list:\n%s", listOut)
	require.Contains(t, string(listOut), fired.ID)
	require.Contains(t, string(listOut), "landed")

	showCmd := exec.Command(bin, "--data-dir", dataDir, "--db", dbPath, "mission", "show", fired.ID)
	showCmd.Env = baseEnv
	showOut, err := showCmd.CombinedOutput()
	require.NoErrorf(t, err, "mission show:\n%s", showOut)
	require.Contains(t, string(showOut), intent)
	require.Contains(t, string(showOut), "fire e2e unit done")
}
