package fleetservice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// missionReportChain is a deterministic, model-free chain: a `tools` task
// calls the unit's mission_report tool with static args, then a noop
// terminator. It never resolves a model, so this proves the report path,
// not inference.
const missionReportChain = `{
  "id": "e2e-mission-report",
  "tasks": [
    {
      "id": "report",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_report",
        "args": {"kind": "result", "summary": "unit reporting from the field"}
      },
      "transition": {"branches": [{"operator": "default", "goto": "done"}]}
    },
    {
      "id": "done",
      "handler": "noop",
      "transition": {"branches": [{"operator": "default", "goto": "end"}]}
    }
  ]
}`

// buildContenoxBin compiles the full contenox binary into t.TempDir() so a
// dispatched unit can be spawned as a real `contenox acp` subprocess.
func buildContenoxBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/cmd/contenox").CombinedOutput()
	require.NoError(t, err, "build contenox:\n%s", out)
	return binPath
}

// TestFleetService_E2E_MissionReportFromDispatchedUnit: a declared unit is
// dispatched on a mission, runs unattended through a real subprocess, and
// files a report on its own mission through the mission_report tool, read
// back here via missionservice.ListReports.
func TestFleetService_E2E_MissionReportFromDispatchedUnit(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: builds the contenox binary and spawns a real ACP subprocess")
	}

	bin := buildContenoxBin(t)

	// Isolate HOME so the unit's $HOME/.contenox and this test's DB handle
	// are one and the same store; the spawned subprocess inherits this
	// process's environment.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	contenoxDir := filepath.Join(tmpHome, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o700))
	dbPath := filepath.Join(contenoxDir, "local.db")

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// The name is deliberately fake: the unit needs a model configured to
	// build its engine, but the mission chain resolves none, so an
	// accidental model call would fail loudly.
	runContenox(t, bin, "config", "set", "default-model", "fake-e2e-model-does-not-exist")

	chainPath := filepath.Join(contenoxDir, "mission-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(missionReportChain), 0o644))

	// A `contenox acp --auto` subprocess (auto = no HITL, so the
	// mission_report tool call runs unattended), sharing this HOME and DB.
	agents := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: "reporter", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agents.Create(ctx, agent))

	stderr := &lockedBuffer{}
	kernel := agentinstance.New(agents, agentinstance.WithStderr(stderr))
	t.Cleanup(func() { _ = kernel.Close() })
	missions := missionservice.New(db)
	svc := New(kernel, agents, missions, nil, tmpHome, libtracker.NoopTracker{})

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "reporter",
		Intent:         "run the mission and report in",
		HITLPolicyName: "default",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.MissionID)

	// The intent-driven first turn runs detached; poll for the report the unit
	// files against its own mission over the shared DB.
	var reports []*missionservice.Report
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		reports, err = missions.ListReports(ctx, result.MissionID, 10)
		require.NoError(t, err)
		if len(reports) > 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	require.Lenf(t, reports, 1,
		"the dispatched unit must file exactly one report on its own mission.\nsubprocess stderr:\n%s", stderr.String())
	require.Equal(t, missionservice.ReportKindResult, reports[0].Kind)
	require.Equal(t, "unit reporting from the field", reports[0].Summary)
	require.Equal(t, result.MissionID, reports[0].MissionID,
		"the report is scoped to the unit's OWN mission, forwarded at session/new")

	// Filing the report stamped mission liveness.
	m, err := missions.Get(ctx, result.MissionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "a filed report is proof of life and heartbeats the mission")
}

// runContenox runs the built binary with args and fails the test on a non-zero
// exit, surfacing combined output. It inherits the process environment (so the
// isolated HOME reaches it), which is how config/seed subcommands land in the
// same $HOME/.contenox the unit reads.
func runContenox(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	require.NoErrorf(t, err, "contenox %v failed:\n%s", args, out)
}
