package agentservice_test

// What a mission's host process leaves behind when it dies. The mission
// record, its unanswered ask, and its suspended run are three durable rows
// with three different owners; this pins the state a fresh process actually
// finds, and which of the three a fresh process reconciles.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestSystem_HostDeath_ReclaimsTheMissionAndKeepsItsWorkAnswerable pins the
// split a fresh process finds after a host dies mid-ask. The mission row is
// unreachable the moment its host is gone — a child subprocess cannot outlive
// its parent — so the mission sweep collects it: abandoned, with the reason
// and a blocker naming the silence. The other two rows are NOT garbage: the
// ask is still pending and answerable at its own ceiling, and the run is
// still checkpointed, so the reclaim costs the work nothing (see the
// companion test).
func TestSystem_HostDeath_ReclaimsTheMissionAndKeepsItsWorkAnswerable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "orphan.db")
	ctx := context.Background()

	a := newAskerInstance(t, dbPath)
	missionID := createMission(t, a.missions)

	// The unit reports liveness, then asks and parks; the host dies here.
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

	// A fresh process, any time later.
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

	// Wind the frozen heartbeat past the bound: the silence a real orphan
	// accumulates while the operator is away.
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

	// The reclaim collects the mission record only. The two rows that still
	// carry work are untouched.
	row, err := b.store.GetHITLApproval(ctx, "call-orphan")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.HITLApprovalPending, row.State, "the question outlives its asker, as designed")
	_, err = b.store.GetChainCheckpoint(ctx, "call-orphan")
	require.NoError(t, err, "and so does the suspended run")
}

// TestSystem_HostDeath_AnsweringTheOrphanStillResumesIt pins the half that
// does hold: the mission record going stale costs the run nothing. The ask is
// still answerable from a fresh process days later, and answering it resumes
// the suspended work — the flagship claim's core, independent of G8.
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
