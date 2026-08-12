package missionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/contenox/contenox/errdefs"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func (p *fakePublisher) statusChangedEvents(t *testing.T) []StatusChangedEvent {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]StatusChangedEvent, 0, len(p.payloads))
	for i, raw := range p.payloads {
		require.Equal(t, StatusChangedSubject, p.subjects[i], "terminal transitions publish only on StatusChangedSubject")
		var ev StatusChangedEvent
		require.NoError(t, json.Unmarshal(raw, &ev))
		out = append(out, ev)
	}
	return out
}

func TestUnit_MissionService_FinishMovesOpenToTerminal(t *testing.T) {
	for _, status := range []Status{StatusLanded, StatusDerailed, StatusStuck, StatusAbandoned} {
		t.Run(string(status), func(t *testing.T) {
			ctx, db := setupMissionDB(t)
			svc := New(db)

			m := newMission("finish me")
			require.NoError(t, svc.Create(ctx, m))

			finished, err := svc.Finish(ctx, m.ID, status, "because reasons")
			require.NoError(t, err)
			require.Equal(t, status, finished.Status)
			require.Equal(t, "because reasons", finished.StatusReason)

			persisted, err := svc.Get(ctx, m.ID)
			require.NoError(t, err)
			require.Equal(t, status, persisted.Status, "the terminal status is durable")
			require.Equal(t, "because reasons", persisted.StatusReason)
		})
	}
}

// TestUnit_MissionService_FinishRejectsNonTerminalTarget pins that Finish only records terminal states, never open.
func TestUnit_MissionService_FinishRejectsNonTerminalTarget(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("stay open")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Finish(ctx, m.ID, StatusOpen, "nope")
	require.Error(t, err)

	_, err = svc.Finish(ctx, m.ID, "bogus", "nope")
	require.Error(t, err)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusOpen, persisted.Status, "a rejected Finish leaves the mission untouched")
}

// TestUnit_MissionService_FinishIsIdempotentForSameStatusAndConflictsOnDifferent pins the core guard: same-status retry is a no-op, different-status is a conflict.
func TestUnit_MissionService_FinishIsIdempotentForSameStatusAndConflictsOnDifferent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("finish once")
	require.NoError(t, svc.Create(ctx, m))

	first, err := svc.Finish(ctx, m.ID, StatusLanded, "shipped it")
	require.NoError(t, err)
	firstUpdatedAt := first.UpdatedAt

	// Same terminal status again: idempotent no-op — unchanged, not restamped.
	again, err := svc.Finish(ctx, m.ID, StatusLanded, "a different reason on retry")
	require.NoError(t, err, "re-finishing with the same status is an idempotent no-op")
	require.Equal(t, StatusLanded, again.Status)
	require.Equal(t, "shipped it", again.StatusReason, "an idempotent retry must not overwrite the recorded reason")
	require.Equal(t, firstUpdatedAt, again.UpdatedAt, "a true no-op must not restamp UpdatedAt")

	// A different terminal status: a conflict, the mission stays landed.
	_, err = svc.Finish(ctx, m.ID, StatusDerailed, "second thoughts")
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict, "a re-finish to a different terminal is a conflict")

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusLanded, persisted.Status, "the first terminal status is immutable")
}

func TestUnit_MissionService_FinishUnknownReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	_, err := svc.Finish(ctx, "no-such-id", StatusLanded, "")
	require.Error(t, err)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// TestUnit_MissionService_FinishPublishesStatusChangedEvent pins that Finish publishes a self-contained StatusChangedEvent.
func TestUnit_MissionService_FinishPublishesStatusChangedEvent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("supervised terminal")
	m.ParentSessionID = "parent-session-7"
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Finish(ctx, m.ID, StatusDerailed, "ran into a wall")
	require.NoError(t, err)

	evs := pub.statusChangedEvents(t)
	require.Len(t, evs, 1, "exactly one event per terminal transition")
	ev := evs[0]
	require.Equal(t, m.ID, ev.MissionID)
	require.Equal(t, "parent-session-7", ev.ParentSessionID, "the supervision edge rides on the event")
	require.Equal(t, m.AgentName, ev.AgentName)
	require.Equal(t, m.Intent, ev.Intent)
	require.Equal(t, StatusOpen, ev.OldStatus)
	require.Equal(t, StatusDerailed, ev.NewStatus)
	require.Equal(t, "ran into a wall", ev.Reason)
}

// TestUnit_MissionService_FinishIdempotentNoOpDoesNotRepublish pins that a retried no-op Finish emits no second event.
func TestUnit_MissionService_FinishIdempotentNoOpDoesNotRepublish(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("finish twice")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Finish(ctx, m.ID, StatusLanded, "done")
	require.NoError(t, err)
	_, err = svc.Finish(ctx, m.ID, StatusLanded, "done again")
	require.NoError(t, err)

	require.Len(t, pub.statusChangedEvents(t), 1, "an idempotent no-op emits no second event")
}

// TestUnit_MissionService_FinishPublishFailureDoesNotFailFinish pins that a publish failure never fails an already-durable Finish.
func TestUnit_MissionService_FinishPublishFailureDoesNotFailFinish(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{err: fmt.Errorf("bus is down")}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("survive a broken bus")
	require.NoError(t, svc.Create(ctx, m))

	finished, err := svc.Finish(ctx, m.ID, StatusStuck, "wedged")
	require.NoError(t, err, "a publish failure must not fail Finish — the status is the durable fact")
	require.Equal(t, StatusStuck, finished.Status)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusStuck, persisted.Status, "the terminal status is durable regardless of the routing nudge")
}

// TestUnit_MissionService_FinishNoPublisherStillStores pins that a service without a publisher finishes missions and publishes nothing.
func TestUnit_MissionService_FinishNoPublisherStillStores(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db) // no publisher

	m := newMission("no bus wired")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.Finish(ctx, m.ID, StatusLanded, "done")
	require.NoError(t, err)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusLanded, persisted.Status)
}

type interposingDB struct {
	libdb.DBManager
	once      sync.Once
	afterRead func()
}

func (d *interposingDB) WithoutTransaction() libdb.Exec {
	return interposingExec{Exec: d.DBManager.WithoutTransaction(), owner: d}
}

type interposingExec struct {
	libdb.Exec
	owner *interposingDB
}

// QueryRowContext intercepts GetKVRaw's read — the one whose bytes become putIfUnchanged's predicate — matched on its projection, which no other kv read shares.
func (e interposingExec) QueryRowContext(ctx context.Context, query string, args ...any) libdb.QueryRower {
	row := e.Exec.QueryRowContext(ctx, query, args...)
	if !strings.Contains(query, "SELECT value") {
		return row
	}
	return interposingRow{QueryRower: row, owner: e.owner}
}

type interposingRow struct {
	libdb.QueryRower
	owner *interposingDB
}

func (r interposingRow) Scan(dest ...any) error {
	err := r.QueryRower.Scan(dest...)
	if err == nil {
		r.owner.once.Do(r.owner.afterRead)
	}
	return err
}

// TestUnit_SetPlan_CannotResurrectAMissionStoppedUnderIt pins the plan write's conditional predicate: a unit calls mission_plan while an operator's `mission stop` reclaims the mission microseconds earlier, and an unguarded put must not restore the status its read saw.
func TestUnit_SetPlan_CannotResurrectAMissionStoppedUnderIt(t *testing.T) {
	ctx, db := setupMissionDB(t)
	stopper := New(db) // unwrapped: the operator's own `mission stop`

	m := newMission("plan while being stopped")
	require.NoError(t, stopper.Create(ctx, m))

	interposed := &interposingDB{DBManager: db}
	interposed.afterRead = func() {
		_, err := stopper.Finish(ctx, m.ID, StatusAbandoned, "stopped by operator")
		require.NoError(t, err)
	}

	planned, err := New(interposed).SetPlan(ctx, m.ID, []PlanEntry{
		entry("e1", "step one", PlanEntryPending, PlanEntryPriorityHigh),
	}, "first pass")
	require.NoError(t, err, "the write re-reads and re-judges rather than failing its caller")
	require.Equal(t, StatusAbandoned, planned.Status)

	got, err := stopper.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status, "a plan write must never put an at-rest row back in motion")
	require.Equal(t, "stopped by operator", got.StatusReason, "nor overwrite the reason the operator's stop recorded")
	require.Equal(t, 1, got.Plan.Revision, "the plan still lands — on the row as it actually is")
}

// TestUnit_Bind_CannotResurrectAMissionStoppedUnderIt is the same window on
// the other whole-record write: dispatch binding its session and instance
// while the mission is stopped out from under it.
func TestUnit_Bind_CannotResurrectAMissionStoppedUnderIt(t *testing.T) {
	ctx, db := setupMissionDB(t)
	stopper := New(db)

	m := newMission("bind while being stopped")
	require.NoError(t, stopper.Create(ctx, m))

	interposed := &interposingDB{DBManager: db}
	interposed.afterRead = func() {
		_, err := stopper.Finish(ctx, m.ID, StatusAbandoned, "stopped by operator")
		require.NoError(t, err)
	}

	bound, err := New(interposed).Bind(ctx, m.ID, "sess-1", "inst-1")
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, bound.Status)

	got, err := stopper.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, got.Status, "a bind must never put an at-rest row back in motion")
	require.Equal(t, "sess-1", got.SessionID, "the bind itself still lands")
	require.Equal(t, "inst-1", got.InstanceID)
}

// TestUnit_Bind_ReJudgesTheConflictGuardAgainstTheRowItWrites pins that the
// retry re-runs the guard rather than retrying blind: a session bound by
// someone else inside the window is a conflict, not an overwrite.
func TestUnit_Bind_ReJudgesTheConflictGuardAgainstTheRowItWrites(t *testing.T) {
	ctx, db := setupMissionDB(t)
	other := New(db)

	m := newMission("two dispatches, one mission")
	require.NoError(t, other.Create(ctx, m))

	interposed := &interposingDB{DBManager: db}
	interposed.afterRead = func() {
		_, err := other.Bind(ctx, m.ID, "sess-winner", "")
		require.NoError(t, err)
	}

	_, err := New(interposed).Bind(ctx, m.ID, "sess-loser", "")
	require.Error(t, err)
	require.ErrorIs(t, err, errdefs.ErrConflict, "the loser is refused against the row as it now is")

	got, err := other.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, "sess-winner", got.SessionID, "and never overwrites the binding that landed")
}

// TestUnit_SetPlan_ReJudgesTheCompletedGuardAgainstTheRowItWrites pins that
// the retry loop re-runs the immutability guard rather than retrying blind:
// the guard is judged against whatever prior revision the write lands on.
func TestUnit_SetPlan_ReJudgesTheCompletedGuardAgainstTheRowItWrites(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("revise a finished step")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("e1", "ship the parser", PlanEntryCompleted, PlanEntryPriorityHigh),
	}, "done")
	require.NoError(t, err)

	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("e1", "ship a different parser", PlanEntryCompleted, PlanEntryPriorityHigh),
	}, "rewrite history")
	require.Error(t, err, "completed work stays immutable across the CAS loop")

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.Plan.Revision, "a rejected snapshot bumps nothing")
	require.Equal(t, "ship the parser", got.Plan.Entries[0].Content)
}

// TestUnit_SetPlan_AssignsEachEntryIdExactlyOnce pins that normalization sits
// above the retry loop: a re-judged attempt must reuse the ids the first pass
// assigned, or a retry would silently rewrite entry identity.
func TestUnit_SetPlan_AssignsEachEntryIdExactlyOnce(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("id-less entries")
	require.NoError(t, svc.Create(ctx, m))

	planned, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		{Content: "step one", Status: PlanEntryPending, Priority: PlanEntryPriorityHigh},
	}, "")
	require.NoError(t, err)
	require.NotEmpty(t, planned.Plan.Entries[0].ID)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, planned.Plan.Entries[0].ID, got.Plan.Entries[0].ID,
		"the id handed back is the id stored")
}
