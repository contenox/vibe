package fleetservice

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func orphanedMission(t *testing.T, ctx context.Context, missions missionservice.Service, intent string) *missionservice.Mission {
	t.Helper()
	m := &missionservice.Mission{Intent: intent, AgentName: "unit", HITLPolicyName: "p.json"}
	require.NoError(t, missions.Create(ctx, m))
	frozen := time.Now().UTC().Add(-missionservice.StaleHeartbeatAfter - time.Hour)
	m.CreatedAt = frozen
	m.LastHeartbeat = &frozen
	require.NoError(t, missions.Update(ctx, m))
	return m
}

func TestUnit_BuildInProcess_ReclaimsWhatADeadHostLeftBehind(t *testing.T) {
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleet-sweep.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	missions := missionservice.New(db)
	orphan := orphanedMission(t, ctx, missions, "left open by a dead host")
	live := &missionservice.Mission{Intent: "fired just now", AgentName: "unit", HITLPolicyName: "p.json"}
	require.NoError(t, missions.Create(ctx, live))

	_, _, stop, err := BuildInProcess(ctx, InProcessDeps{
		DB:       db,
		Bus:      libbus.NewInMem(),
		Missions: missions,
	})
	require.NoError(t, err)
	defer stop()

	got, err := missions.Get(ctx, orphan.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusAbandoned, got.Status,
		"a host comes up and collects the wreckage of the one before it")

	got, err = missions.Get(ctx, live.ID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, got.Status, "a mission fired moments ago is not wreckage")
}

// TestUnit_SweepAbandonedMissions_NeverFailsTheHost pins the register the
// startup sweep runs in: a fleet that cannot collect yesterday's wreckage must
// still come up.
func TestUnit_SweepAbandonedMissions_NeverFailsTheHost(t *testing.T) {
	require.NotPanics(t, func() {
		sweepAbandonedMissions(context.Background(), nil, nil)
	}, "no mission service and no tracker is a no-op, not a crash")
}
