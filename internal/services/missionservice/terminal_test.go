package missionservice

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/contenox/beam/internal/errdefs"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/stretchr/testify/require"
)

// statusChangedEvents decodes every StatusChangedEvent this publisher captured,
// asserting each rode StatusChangedSubject. It is the terminal-transition twin
// of report_events_test.go's events() helper.
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

// ─── Finish: the guarded terminal transition ───────────────────────────────

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

// Finish refuses a non-terminal target: you cannot Finish a mission back to
// open (or to any non-terminal value) — Finish only records END states.
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

// The core guard: a mission already terminal is immutable through Finish. A
// second Finish naming the SAME status is an idempotent no-op (safe retry); a
// DIFFERENT terminal status is a conflict (a landed mission never becomes
// derailed).
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

// ─── status_changed events ─────────────────────────────────────────────────

// A successful Finish publishes a self-contained StatusChangedEvent carrying the
// supervision edge, the old→new transition, and the reason.
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

// The idempotent no-op path publishes nothing: a retried Finish that changed
// nothing must not emit a second status_changed event.
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

// The best-effort invariant: a publish failure never turns a durably-recorded
// terminal transition into a failed Finish.
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

// The publisher is optional: a service built without one finishes missions and
// simply publishes nothing.
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
