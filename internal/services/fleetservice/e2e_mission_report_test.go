package fleetservice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

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

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	contenoxDir := filepath.Join(tmpHome, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o700))
	dbPath := filepath.Join(contenoxDir, "local.db")

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	runContenox(t, bin, "config", "set", "default-model", "fake-e2e-model-does-not-exist")

	chainPath := filepath.Join(contenoxDir, "mission-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(missionReportChain), 0o644))

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

	m, err := missions.Get(ctx, result.MissionID)
	require.NoError(t, err)
	require.NotNil(t, m.LastHeartbeat, "a filed report is proof of life and heartbeats the mission")
}

func runContenox(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	require.NoErrorf(t, err, "contenox %v failed:\n%s", args, out)
}
