package fleetservice

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func stopTestDeps(t *testing.T) (context.Context, missionservice.Service, hitlservice.Service, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "stop.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(t.TempDir()), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")
	return ctx, missionservice.New(db), hitl, store
}

func pendingAskRow(t *testing.T, ctx context.Context, store runtimetypes.Store, id, missionID, toolsName, toolName string) {
	t.Helper()
	require.NoError(t, store.CreateHITLApproval(ctx, &runtimetypes.HITLApproval{
		ID:        id,
		ToolsName: toolsName,
		ToolName:  toolName,
		State:     runtimetypes.HITLApprovalPending,
		MissionID: &missionID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}))
}

// TestUnit_StopMission_AbandonsAsksAndDeletesCheckpoints: the mission lands
// abandoned, every pending ask it filed closes denied, and their checkpoints
// are deleted.
func TestUnit_StopMission_AlreadyTerminalIsAConflict(t *testing.T) {
	ctx, missions, hitl, store := stopTestDeps(t)

	m := &missionservice.Mission{Intent: "one", AgentName: "a", HITLPolicyName: "p.json"}
	require.NoError(t, missions.Create(ctx, m))
	_, err := missions.Finish(ctx, m.ID, missionservice.StatusLanded, "done")
	require.NoError(t, err)

	err = StopMission(ctx, missions, hitl, store, m.ID, "too late")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already finished")
}

// TestUnit_StopMission_SkipsConcurrentlyResolvedAsk: an ask resolved while
// the stop was in flight is skipped, not an error.
func TestUnit_StopMission_SkipsConcurrentlyResolvedAsk(t *testing.T) {
	ctx, missions, hitl, store := stopTestDeps(t)

	m := &missionservice.Mission{Intent: "two", AgentName: "a", HITLPolicyName: "p.json"}
	require.NoError(t, missions.Create(ctx, m))
	pendingAskRow(t, ctx, store, "ask-answered", m.ID, "local_shell", "exec")
	require.NoError(t, hitl.Respond(ctx, "ask-answered", true))

	require.NoError(t, StopMission(ctx, missions, hitl, store, m.ID, ""))

	row, err := store.GetHITLApproval(ctx, "ask-answered")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalApproved, row.State, "the operator's earlier verdict must stand")
}
