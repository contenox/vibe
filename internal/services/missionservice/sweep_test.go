package missionservice

// What the mission garbage collector must and must not touch. A mission unit
// dies with the process that fired it; these pin that the row follows, that a
// live-but-slow mission does not, and that a reclaim and a normal Finish
// racing on one mission produce exactly one terminal state.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// staleMission creates an open mission whose liveness stopped silence ago —
// both stamps backdated, since lastLiveness takes the later of the two.
func staleMission(t *testing.T, ctx context.Context, svc Service, intent string, silence time.Duration) *Mission {
	t.Helper()
	m := newMission(intent)
	require.NoError(t, svc.Create(ctx, m))
	backdated := time.Now().UTC().Add(-silence)
	m.CreatedAt = backdated
	m.LastHeartbeat = &backdated
	require.NoError(t, svc.Update(ctx, m))
	return m
}

func TestUnit_SweepAbandoned_ReclaimsAMissionWhoseHostIsGone(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := staleMission(t, ctx, svc, "outlive my host", StaleHeartbeatAfter+time.Hour)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status, "an unreachable mission comes to rest at the status mission stop already writes")
	require.Contains(t, got.StatusReason, AbandonedBySweepReason,
		"the reason distinguishes a reclaimed mission from one an operator stopped by hand")
	require.Contains(t, got.StatusReason, "no heartbeat for", "and names the silence that justified it")
}

// TestUnit_SweepAbandoned_LeavesTheDurableRecordAnOperatorReads pins the
// second half of the reclaim: the row alone says "abandoned", the report says
// why — and routes on to the operator inbox, its host being gone.
func TestUnit_SweepAbandoned_LeavesTheDurableRecordAnOperatorReads(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := staleMission(t, ctx, svc, "leave a trail", StaleHeartbeatAfter*2)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1, "a reclaim files exactly one blocker")
	require.Equal(t, ReportKindBlocker, reports[0].Kind)
	require.Equal(t, abandonedReportSummary, reports[0].Summary)
	require.Contains(t, reports[0].Detail, "No heartbeat for")
}

func TestUnit_SweepAbandoned_LeavesALiveMissionAlone(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	// Silent for most of the bound but not past it: a slow host, not a dead one.
	fresh := staleMission(t, ctx, svc, "still working", StaleHeartbeatAfter-time.Minute)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, reclaimed, "a mission inside the staleness bound is never reaped")

	got, err := svc.Get(ctx, fresh.ID)
	require.NoError(t, err)
	require.Equal(t, StatusOpen, got.Status)
	require.Empty(t, got.StatusReason)

	reports, err := svc.ListReports(ctx, fresh.ID, 10)
	require.NoError(t, err)
	require.Empty(t, reports, "and carries no blocker explaining an ending that never happened")
}

// TestUnit_SweepAbandoned_NeverStampedAMissionIsMeasuredFromCreation pins that
// a host dying before its unit's first turn is reclaimed too, rather than
// staying open forever on a heartbeat that was never written.
func TestUnit_SweepAbandoned_NeverStampedAMissionIsMeasuredFromCreation(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("died before the first turn")
	require.NoError(t, svc.Create(ctx, m))
	m.CreatedAt = time.Now().UTC().Add(-StaleHeartbeatAfter - time.Hour)
	require.NoError(t, svc.Update(ctx, m))

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status)
	require.Nil(t, got.LastHeartbeat, "measured from creation; no liveness fact is invented")
}

// TestUnit_SweepAbandoned_IsIdempotent pins that repeated sweeps converge:
// the first reclaims, the rest find a terminal row and do nothing to it.
func TestUnit_SweepAbandoned_IsIdempotent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := staleMission(t, ctx, svc, "sweep me twice", StaleHeartbeatAfter*3)

	first, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, first)

	after, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		again, err := svc.SweepAbandoned(ctx)
		require.NoError(t, err)
		require.Equal(t, 0, again, "a reclaimed mission is terminal and never a candidate again")
	}

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, after.Status, got.Status)
	require.Equal(t, after.StatusReason, got.StatusReason, "no restamp, no second reason")
	require.Equal(t, after.UpdatedAt, got.UpdatedAt)

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1, "and exactly one blocker, not one per sweep")
}

// TestUnit_SweepAbandoned_LeavesAManuallyStoppedMissionUntouched pins that the
// operator's own kill is not re-written by the collector, however long ago it
// stopped reporting: the sweep only ever moves an OPEN row.
func TestUnit_SweepAbandoned_LeavesAManuallyStoppedMissionUntouched(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := staleMission(t, ctx, svc, "stopped by hand", StaleHeartbeatAfter*4)
	stopped, err := svc.Finish(ctx, m.ID, StatusAbandoned, "stopped by operator")
	require.NoError(t, err)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, reclaimed)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped by operator", got.StatusReason, "the operator's reason survives; the sweep does not overwrite it")
	require.Equal(t, stopped.UpdatedAt, got.UpdatedAt)

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Empty(t, reports, "a stopped mission gets no reclaim blocker")
}

// TestUnit_SweepAbandoned_LeavesEveryOtherTerminalStatusAlone is the same
// guard across the closed terminal set, so a landed mission read a week later
// is not relabelled by the collector.
func TestUnit_SweepAbandoned_LeavesEveryOtherTerminalStatusAlone(t *testing.T) {
	for _, status := range []Status{StatusLanded, StatusDerailed, StatusStuck} {
		t.Run(string(status), func(t *testing.T) {
			ctx, db := setupMissionDB(t)
			svc := New(db)

			m := staleMission(t, ctx, svc, "finished long ago", StaleHeartbeatAfter*4)
			_, err := svc.Finish(ctx, m.ID, status, "done")
			require.NoError(t, err)

			reclaimed, err := svc.SweepAbandoned(ctx)
			require.NoError(t, err)
			require.Equal(t, 0, reclaimed)

			got, err := svc.Get(ctx, m.ID)
			require.NoError(t, err)
			require.Equal(t, status, got.Status)
		})
	}
}

// TestUnit_SweepAbandoned_RacesFinishToExactlyOneTerminalState is the -race
// test for the conditional write: a unit landing its mission at the same
// moment the collector reclaims it. Whoever wins, the row ends in exactly one
// terminal state, with the reason that matches it — never a reclaim reason
// over a landed status, and never a reclaim blocker on a mission that landed.
func TestUnit_SweepAbandoned_RacesFinishToExactlyOneTerminalState(t *testing.T) {
	for i := 0; i < 20; i++ {
		ctx, db := setupMissionDB(t)
		svc := New(db)
		m := staleMission(t, ctx, svc, "land or be reclaimed", StaleHeartbeatAfter*2)

		var wg sync.WaitGroup
		var sweepErr, finishErr error
		var reclaimed int
		wg.Add(2)
		go func() {
			defer wg.Done()
			reclaimed, sweepErr = svc.SweepAbandoned(ctx)
		}()
		go func() {
			defer wg.Done()
			_, finishErr = svc.Finish(ctx, m.ID, StatusLanded, "the unit got there first")
		}()
		wg.Wait()

		require.NoError(t, sweepErr)
		got, err := svc.Get(ctx, m.ID)
		require.NoError(t, err)
		require.True(t, isTerminalStatus(got.Status), "the mission ends at rest either way")

		reports, err := svc.ListReports(ctx, m.ID, 10)
		require.NoError(t, err)

		switch got.Status {
		case StatusLanded:
			require.NoError(t, finishErr, "the winner's own call reports success")
			require.Equal(t, "the unit got there first", got.StatusReason)
			require.Equal(t, 0, reclaimed, "the collector counts only writes that landed")
			require.Empty(t, reports, "a landed mission carries no reclaim blocker")
		case StatusAbandoned:
			require.Equal(t, 1, reclaimed)
			require.Contains(t, got.StatusReason, AbandonedBySweepReason)
			require.Error(t, finishErr, "the loser is refused, not silently dropped")
			require.Len(t, reports, 1)
		default:
			t.Fatalf("mission reached an unexpected terminal state %q", got.Status)
		}
	}
}

// TestUnit_Heartbeat_NeverResurrectsAReclaimedMission pins the other half of
// the race: liveness written by a unit that outlived the sweep's judgement
// must not put an at-rest row back in motion.
func TestUnit_Heartbeat_NeverResurrectsAReclaimedMission(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := staleMission(t, ctx, svc, "beat after death", StaleHeartbeatAfter*2)
	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	beaten, err := svc.Heartbeat(ctx, m.ID, "")
	require.NoError(t, err, "a late heartbeat is a no-op, not a failure its caller must handle")
	require.Equal(t, StatusAbandoned, beaten.Status)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status, "the terminal status is durable against a late heartbeat")
	require.Contains(t, got.StatusReason, AbandonedBySweepReason)
}

// TestUnit_SweepAbandoned_ReclaimsAcrossPagesOfMissions pins that the scan
// walks past one page: orphans are the OLDEST rows, and the prefix scan is
// newest-first, so a single page would systematically miss them.
func TestUnit_SweepAbandoned_ReclaimsAcrossPagesOfMissions(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	stale := staleMission(t, ctx, svc, "the oldest orphan", StaleHeartbeatAfter*2)
	for i := 0; i < scanPageSize+5; i++ {
		m := newMission("live mission")
		require.NoError(t, svc.Create(ctx, m))
	}

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed, "only the orphan, however deep in the scan it sits")

	got, err := svc.Get(ctx, stale.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status)
}

// TestUnit_StaleHeartbeatAfter_HasHeadroomOverTheLongestLegitimateSilence
// pins the bound's justification, not its number: liveness is stamped per
// turn, and the longest a turn can legitimately block is one ask parked on
// hitlservice's serve-level ceiling.
func TestUnit_StaleHeartbeatAfter_HasHeadroomOverTheLongestLegitimateSilence(t *testing.T) {
	require.Equal(t, time.Hour, heartbeatCeiling,
		"the ceiling tracks hitlservice.DefaultApprovalCeiling; if that moves, this must")
	require.Greater(t, staleHeartbeatMultiple, 1,
		"a bound at exactly one ceiling would reap a host that parked on an ask and lived")
	require.Equal(t, staleHeartbeatMultiple*heartbeatCeiling, StaleHeartbeatAfter)
}

// ─── the bound a mission's own park widens ─────────────────────────────────
//
// The serve-level ceiling is not the only thing that parks a unit. A policy
// rule may set its own timeout_s, and nothing heartbeats while a unit waits on
// an ask — the last stamp is the end of the turn that raised it. A flat bound
// would therefore reap a live, correctly-parked mission, making the
// mission_finish its resumed run eventually calls a permanent conflict: a
// durable verdict wrong forever.

// maxRuleTimeout mirrors hitlservice's own seven-day ceiling on a policy
// rule's timeout_s (hitlservice.maxRuleTimeoutS — named in prose, not
// imported, since hitlservice imports this package). It is the longest an
// envelope may legitimately park a unit on one ask.
const maxRuleTimeout = 7 * 24 * time.Hour

// serveCeiling mirrors hitlservice.DefaultApprovalCeiling, the window an
// attention ask carries when no rule bounds it — the case StaleHeartbeatAfter
// was already sized for.
const serveCeiling = time.Hour

// parkedAsk writes one durable ask attributed to missionID: raised raisedAgo
// ago, in state, configured to wait window before its deadline.
func parkedAsk(t *testing.T, ctx context.Context, db libdb.DBManager, missionID string, state runtimetypes.HITLApprovalState, raisedAgo, window time.Duration) string {
	t.Helper()
	raised := time.Now().UTC().Add(-raisedAgo)
	row := &runtimetypes.HITLApproval{
		ID:          uuid.NewString(),
		ToolsName:   "mission",
		ToolName:    "mission_ask_attention",
		ArgsSummary: "which branch should I target?",
		OnTimeout:   "deny",
		State:       state,
		MissionID:   &missionID,
		CreatedAt:   raised,
		ExpiresAt:   raised.Add(window),
	}
	if state != runtimetypes.HITLApprovalPending {
		resolved := raised.Add(window / 2)
		row.ResolvedAt = &resolved
	}
	require.NoError(t, runtimetypes.New(db.WithoutTransaction()).CreateHITLApproval(ctx, row))
	return row.ID
}

// TestUnit_SweepAbandoned_LeavesAMissionInsideItsOwnParkWindow pins the bound
// StaleHeartbeatAfter alone cannot express, against the ceiling it comes from:
// a rule may park a unit for longer than the floor, so that park's own window
// is what the mission's silence is judged by.
func TestUnit_SweepAbandoned_LeavesAMissionInsideItsOwnParkWindow(t *testing.T) {
	require.Greater(t, maxRuleTimeout, time.Duration(StaleHeartbeatAfter),
		"the widening exists only because a rule may park a unit past the floor; "+
			"if StaleHeartbeatAfter ever covers the whole timeout_s ceiling, revisit whether it is still needed")

	ctx, db := setupMissionDB(t)
	svc := New(db)

	// Hour 7 of a seven-day park: past the floor, and still waiting.
	silence := StaleHeartbeatAfter + time.Hour
	m := staleMission(t, ctx, svc, "parked on a seven-day ask", silence)
	parkedAsk(t, ctx, db, m.ID, runtimetypes.HITLApprovalPending, silence, maxRuleTimeout)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, reclaimed, "a mission inside its own park window is silent because it is waiting, not gone")

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusOpen, got.Status,
		"so the answer that arrives at hour 8 resumes real work, and its mission_finish still lands")

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Empty(t, reports, "and no blocker claims an ending that never happened")
}

// TestUnit_SweepAbandoned_ReclaimsOnceTheParkWindowIsSpent pins the other
// side: the widening is that park's window and nothing wider.
func TestUnit_SweepAbandoned_ReclaimsOnceTheParkWindowIsSpent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	silence := maxRuleTimeout + time.Hour
	m := staleMission(t, ctx, svc, "nobody ever answered", silence)
	parkedAsk(t, ctx, db, m.ID, runtimetypes.HITLApprovalPending, silence, maxRuleTimeout)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed, "silence past even the longest park it could be waiting on is silence again")

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status)

	reports, err := svc.ListReports(ctx, m.ID, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Contains(t, reports[0].Detail, formatSilence(maxRuleTimeout),
		"the blocker names the bound actually applied, not the floor")
}

// TestUnit_SweepAbandoned_AParkInsideTheFloorWidensNothing pins that the floor
// still governs the ordinary case: an ask bounded by hitlservice's serve-level
// ceiling is exactly what StaleHeartbeatAfter was sized for, so a host that
// died mid-ask is still collected on schedule.
func TestUnit_SweepAbandoned_AParkInsideTheFloorWidensNothing(t *testing.T) {
	require.Less(t, serveCeiling, time.Duration(StaleHeartbeatAfter),
		"the floor is sized as a multiple of the serve-level ceiling; see heartbeatCeiling")

	ctx, db := setupMissionDB(t)
	svc := New(db)

	silence := StaleHeartbeatAfter + time.Hour
	m := staleMission(t, ctx, svc, "host died mid-ask", silence)
	parkedAsk(t, ctx, db, m.ID, runtimetypes.HITLApprovalPending, silence, serveCeiling)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status)
}

// TestUnit_SweepAbandoned_AResolvedParkWidensNothing pins that only an OPEN
// park widens the bound: a question already answered explains no further
// silence, however long its window was.
func TestUnit_SweepAbandoned_AResolvedParkWidensNothing(t *testing.T) {
	for _, state := range []runtimetypes.HITLApprovalState{
		runtimetypes.HITLApprovalApproved,
		runtimetypes.HITLApprovalDenied,
		runtimetypes.HITLApprovalExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx, db := setupMissionDB(t)
			svc := New(db)

			silence := StaleHeartbeatAfter + time.Hour
			m := staleMission(t, ctx, svc, "asked once, then went quiet", silence)
			parkedAsk(t, ctx, db, m.ID, state, silence, maxRuleTimeout)

			reclaimed, err := svc.SweepAbandoned(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, reclaimed)

			got, err := svc.Get(ctx, m.ID)
			require.NoError(t, err)
			require.Equal(t, StatusAbandoned, got.Status)
		})
	}
}

// TestUnit_SweepAbandoned_AnotherMissionsParkIsNoShield pins that the widening
// is scoped to the mission's own asks: one parked mission must not keep the
// whole board un-collectable.
func TestUnit_SweepAbandoned_AnotherMissionsParkIsNoShield(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	silence := StaleHeartbeatAfter * 2
	orphan := staleMission(t, ctx, svc, "host is gone", silence)
	parked := staleMission(t, ctx, svc, "waiting on a human", silence)
	parkedAsk(t, ctx, db, parked.ID, runtimetypes.HITLApprovalPending, silence, maxRuleTimeout)

	reclaimed, err := svc.SweepAbandoned(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed, "exactly the mission with no park of its own to wait on")

	gotOrphan, err := svc.Get(ctx, orphan.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, gotOrphan.Status)

	gotParked, err := svc.Get(ctx, parked.ID)
	require.NoError(t, err)
	require.Equal(t, StatusOpen, gotParked.Status)
}

// TestUnit_PutIfUnchanged_RefusesAWriteBuiltOnAStaleRead is the store-level
// predicate on its own: the snapshot IS the condition, so a decision taken
// from a read that has since moved on cannot land.
func TestUnit_PutIfUnchanged_RefusesAWriteBuiltOnAStaleRead(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db).(*service)

	m := newMission("cas me")
	require.NoError(t, svc.Create(ctx, m))

	read, snapshot, err := svc.getWithSnapshot(ctx, m.ID)
	require.NoError(t, err)

	// Someone else writes between the read and the write.
	_, err = svc.Heartbeat(ctx, m.ID, "")
	require.NoError(t, err)

	read.Status = StatusAbandoned
	read.StatusReason = "built on a stale read"
	err = svc.putIfUnchanged(ctx, read, snapshot)
	require.ErrorIs(t, err, libdb.ErrNotFound, "a moved row refuses the write rather than taking it")

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusOpen, got.Status)
	require.Empty(t, got.StatusReason)
}

// TestUnit_AbandonedReason_ReadsDifferentlyFromAnOperatorStop pins the one
// fact an operator needs from the row alone: which of the two wrote it.
func TestUnit_AbandonedReason_ReadsDifferentlyFromAnOperatorStop(t *testing.T) {
	reason := abandonedReason(7 * time.Hour)
	require.True(t, strings.HasPrefix(reason, AbandonedBySweepReason))
	require.Contains(t, reason, "7h0m0s")
	require.NotContains(t, reason, "operator")
}
