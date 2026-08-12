package agentservice_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

// TestSystem_HostDeath_ReclaimsTheMissionAndKeepsItsWorkAnswerable pins the split a fresh process finds after a host dies mid-ask: the mission sweep collects the now-unreachable mission row as abandoned, while the still-pending ask and still-checkpointed run are left untouched.
func TestSystem_HostDeath_ReclaimsTheMissionAndKeepsItsWorkAnswerable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "orphan.db")
	ctx := context.Background()

	a := newAskerInstance(t, dbPath)
	missionID := createMission(t, a.missions)

	_, err := a.missions.Heartbeat(ctx, missionID, "")
	require.NoError(t, err)
	unitCtx := missiontools.WithMissionID(ctx, missionID)
	resp, err := a.agent.Prompt(unitCtx, agentservice.PromptRequest{
		InputValue: attentionInput("call-orphan"),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	a.close()

	b := newAskerInstance(t, dbPath)
	defer b.close()

	m, err := b.missions.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, m.Status, "the row survives its host, still open")
	require.NotNil(t, m.LastHeartbeat, "its liveness fact is durable — and now frozen")

	// Nothing has been silent long enough yet, so neither sweeper fires.
	swept, err := b.hitl.SweepExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, swept, "the ask's ceiling has not passed")
	reclaimed, err := b.missions.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, reclaimed, "and the mission's staleness bound has not either")

	// winds the frozen heartbeat past the bound to simulate the silence a real orphan accumulates while the operator is away
	m.CreatedAt = m.CreatedAt.Add(-missionservice.StaleHeartbeatAfter - time.Hour)
	frozen := m.CreatedAt
	m.LastHeartbeat = &frozen
	require.NoError(t, b.missions.Update(ctx, m))

	reclaimed, err = b.missions.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed, "a heartbeat that will never advance is what makes the row collectable")

	m, err = b.missions.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusAbandoned, m.Status,
		"the mission comes to rest without an operator having to run `contenox mission stop`")
	require.Contains(t, m.StatusReason, missionservice.AbandonedBySweepReason,
		"and says it was reclaimed, not stopped by hand")

	reports, err := b.missions.ListReports(ctx, missionID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1, "the operator is not left guessing why it ended")
	require.Equal(t, missionservice.ReportKindBlocker, reports[0].Kind)

	// the reclaim collects the mission record only; the two rows that still carry work are untouched
	row, err := b.store.GetHITLApproval(ctx, "call-orphan")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the question outlives its asker, as designed")
	_, err = b.store.GetChainCheckpoint(ctx, "call-orphan")
	require.NoError(t, err, "and so does the suspended run")
}

// TestSystem_HostDeath_AnsweringTheOrphanStillResumesIt pins that a stale mission record costs the run nothing: the ask stays answerable from a fresh process, and answering it resumes the suspended work.
func TestSystem_HostDeath_AnsweringTheOrphanStillResumesIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "orphan-answer.db")
	ctx := context.Background()

	a := newAskerInstance(t, dbPath)
	missionID := createMission(t, a.missions)
	unitCtx := missiontools.WithMissionID(ctx, missionID)
	resp, err := a.agent.Prompt(unitCtx, agentservice.PromptRequest{
		InputValue: attentionInput("call-orphan2"),
		InputType:  taskengine.DataTypeChatHistory,
		Chain:      attentionChain(),
	})
	require.NoError(t, err)
	require.Equal(t, agentservice.StopSuspended, resp.StopReason)
	a.close()

	b := newAskerInstance(t, dbPath)
	defer b.close()
	require.NoError(t, b.hitl.Answer(ctx, "call-orphan2", "the runtime repo"))

	_, err = b.store.GetChainCheckpoint(ctx, "call-orphan2")
	require.ErrorIs(t, err, libdb.ErrNotFound, "the answer carried the orphaned run to completion")

	m, err := b.missions.Get(ctx, missionID)
	require.NoError(t, err)
	require.Equal(t, missionservice.StatusOpen, m.Status,
		"the resumed run completes without ever moving the mission out of open")
}
