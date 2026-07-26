package fleetservice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// missionReportChain is a deterministic, model-free chain the dispatched unit
// runs as its first (and only) turn: a `tools` task that calls the unit's
// mission_report tool with static args, then a noop terminator. It never touches
// the model — the fake default-model the unit is configured with would fail
// loudly if any task tried to resolve one, which is the point: this proves the
// report path, not inference. The mission_report tool is the unit's OWN local
// provider (registered in `contenox acp`), scoped to the mission id the
// dispatcher forwarded at session/new.
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
// dispatched unit can be spawned as a real `contenox acp` subprocess. It is the
// heavier sibling of buildStubAgentBin (which builds only the hermetic
// stub-agent) — the mission-tools slice must prove the tool reaches a real ACP
// unit, not a stub.
func buildContenoxBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	out, err := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/cmd/contenox").CombinedOutput()
	require.NoError(t, err, "build contenox:\n%s", out)
	return binPath
}

// TestFleetService_E2E_MissionReportFromDispatchedUnit is the acceptance for the
// mission-tools slice, end to end through a REAL subprocess: a declared unit is
// dispatched on a mission, runs unattended, and files a report on ITS OWN
// mission through the mission_report tool — which lands in the shared mission
// store and is read back here via missionservice.ListReports. This proves the
// whole seam the slice builds: the dispatcher forwards the mission id at
// session/new (`_meta`), the unit's ACP agent binds it into the session
// (envelope at construction), and the unit's mission_report tool reports against
// exactly that mission — over a SQLite DB shared by parent and unit because both
// are the same binary rooted at the same $HOME/.contenox.
func TestFleetService_E2E_MissionReportFromDispatchedUnit(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: builds the contenox binary and spawns a real ACP subprocess")
	}

	bin := buildContenoxBin(t)

	// Isolate HOME so the unit's $HOME/.contenox (its DB, chain, and policies)
	// and this test's DB handle are one and the same store. The spawned
	// subprocess inherits this process's environment, so the t.Setenv HOME
	// reaches it.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	contenoxDir := filepath.Join(tmpHome, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o700))
	dbPath := filepath.Join(contenoxDir, "local.db")

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Seed the default model through the REAL CLI. The name is deliberately fake:
	// the unit needs A model configured to build its engine and serve session/new,
	// but the mission chain resolves none, so an accidental model call would fail
	// loudly rather than silently pass.
	runContenox(t, bin, "config", "set", "default-model", "fake-e2e-model-does-not-exist")

	// Seed the deterministic mission chain the unit runs (via CONTENOX_ACP_CHAIN_PATH below).
	chainPath := filepath.Join(contenoxDir, "mission-chain.json")
	require.NoError(t, os.WriteFile(chainPath, []byte(missionReportChain), 0o644))

	// Declare the unit: a `contenox acp --auto` subprocess (auto = no HITL, so the
	// mission_report tool call runs unattended) running the mission chain, sharing
	// this HOME and therefore this DB.
	agents := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: "reporter", Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Args:      []string{"acp", "--auto"},
		Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": chainPath},
	}))
	require.NoError(t, agents.Create(ctx, agent))

	// A concurrency-safe stderr sink (the package's lockedBuffer, as every other
	// e2e here uses): the subprocess's stderr-reader goroutine writes it while the
	// assertion messages below read String(), and a plain bytes.Buffer would race.
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

	// Filing the report stamped mission liveness — the heartbeat rides meaningful activity.
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
