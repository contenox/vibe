package contenoxcli

// What an operator actually sees for a mission the runtime reclaimed, read
// back through the same mission verbs they would run.

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// TestUnit_MissionSurface_ShowsAReclaimedMissionAsAbandonedWithItsReason pins
// the operator-facing half of the reclaim: `mission list` shows it at rest
// rather than open forever, and `mission show` names why it ended and carries
// the blocker, so an abandoned mission never reads like a stopped one.
func TestUnit_MissionSurface_ShowsAReclaimedMissionAsAbandonedWithItsReason(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mission-reclaim-cli.db")
	cmd := testCobraCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", dbPath))
	require.NoError(t, cmd.Root().PersistentFlags().Set("data-dir", filepath.Join(t.TempDir(), ".contenox")))

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	missions := missionservice.New(db)

	orphan := &missionservice.Mission{Intent: "review the open PR", AgentName: "agent-reviewer", HITLPolicyName: "hitl-policy-default.json"}
	require.NoError(t, missions.Create(ctx, orphan))
	frozen := time.Now().UTC().Add(-missionservice.StaleHeartbeatAfter - time.Hour)
	orphan.CreatedAt = frozen
	orphan.LastHeartbeat = &frozen
	require.NoError(t, missions.Update(ctx, orphan))

	reclaimed, err := reclaimAbandonedMissions(ctx, db, "")
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)
	require.NoError(t, db.Close())

	require.NoError(t, missionListCmd.RunE(cmd, nil))
	listed := out.String()
	require.Contains(t, listed, orphan.ID)
	require.Contains(t, listed, string(missionservice.StatusAbandoned))

	out.Reset()
	require.NoError(t, missionShowCmd.RunE(cmd, []string{orphan.ID}))
	shown := out.String()
	require.Contains(t, shown, "Status:    abandoned — "+missionservice.AbandonedBySweepReason,
		"the status line itself says the runtime reclaimed it, not an operator")
	require.Contains(t, shown, "[blocker] Mission reclaimed: its host process is gone.")
}
