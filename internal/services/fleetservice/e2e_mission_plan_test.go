package fleetservice

import (
	"context"
	"os"
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

// missionPlanChain is a deterministic, model-free chain: a `tools` task
// calls the unit's mission_plan tool with a full-snapshot plan (entries
// carried as a JSON string), then a noop terminator. It never resolves a
// model, proving the plan grant reaches a real unit, not inference.
const missionPlanChain = `{
  "id": "e2e-mission-plan",
  "tasks": [
    {
      "id": "plan",
      "handler": "tools",
      "tools": {
        "name": "mission",
        "tool_name": "mission_plan",
        "args": {
          "entries": "[{\"content\":\"survey the codebase\",\"status\":\"in_progress\",\"priority\":\"high\"},{\"content\":\"port the hot loop\",\"status\":\"pending\",\"priority\":\"medium\"}]",
          "explanation": "first cut from the field"
        }
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

// TestFleetService_E2E_MissionPlanFromDispatchedUnit is the plan-channel
// sibling of TestFleetService_E2E_MissionReportFromDispatchedUnit: a
// declared unit is dispatched on a mission, runs unattended through a real
// subprocess, and writes its plan through the mission_plan tool, read back
// here via missionservice.Get.
func TestFleetService_E2E_MissionPlanFromDispatchedUnit(t *testing.T) {
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
	require.NoError(t, os.WriteFile(chainPath, []byte(missionPlanChain), 0o644))

	agents := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: "planner", Enabled: true}
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
		AgentName:      "planner",
		Intent:         "plan the migration and report the shape",
		HITLPolicyName: "default",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.MissionID)

	// The intent-driven first turn runs detached; poll for the plan the unit
	// writes onto its own mission over the shared DB.
	var m *missionservice.Mission
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		m, err = missions.Get(ctx, result.MissionID)
		require.NoError(t, err)
		if m.Plan.Revision > 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	require.Equalf(t, 1, m.Plan.Revision,
		"the dispatched unit must write exactly one plan revision on its own mission.\nsubprocess stderr:\n%s", stderr.String())
	require.Len(t, m.Plan.Entries, 2)
	require.Equal(t, "survey the codebase", m.Plan.Entries[0].Content)
	require.Equal(t, missionservice.PlanEntryInProgress, m.Plan.Entries[0].Status)
	require.Equal(t, missionservice.PlanEntryPriorityHigh, m.Plan.Entries[0].Priority)
	require.Equal(t, "port the hot loop", m.Plan.Entries[1].Content)
	require.Equal(t, "first cut from the field", m.Plan.Explanation)
	// SetPlan assigned ids to the id-less entries — the id echo the projection and
	// the next revision both rely on.
	require.NotEmpty(t, m.Plan.Entries[0].ID)
	require.NotEmpty(t, m.Plan.Entries[1].ID)

	// Writing the plan stamped mission liveness.
	require.NotNil(t, m.LastHeartbeat, "a written plan is proof of life and heartbeats the mission")
}
