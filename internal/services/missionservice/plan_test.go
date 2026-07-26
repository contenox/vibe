package missionservice

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/stretchr/testify/require"
)

// planRevisedEvents decodes every PlanRevisedEvent this publisher captured,
// asserting each rode PlanRevisedSubject.
func (p *fakePublisher) planRevisedEvents(t *testing.T) []PlanRevisedEvent {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PlanRevisedEvent, 0, len(p.payloads))
	for i, raw := range p.payloads {
		require.Equal(t, PlanRevisedSubject, p.subjects[i], "plan revisions publish only on PlanRevisedSubject")
		var ev PlanRevisedEvent
		require.NoError(t, json.Unmarshal(raw, &ev))
		out = append(out, ev)
	}
	return out
}

// entry is a terse PlanEntry builder for the tests below.
func entry(id, content string, status PlanEntryStatus, priority PlanEntryPriority) PlanEntry {
	return PlanEntry{ID: id, Content: content, Status: status, Priority: priority}
}

// ─── validatePlan() shape matrix ────────────────────────────────────────────

func TestUnit_ValidatePlan(t *testing.T) {
	ok := func(content string) []PlanEntry {
		return []PlanEntry{entry("e1", content, PlanEntryPending, PlanEntryPriorityMedium)}
	}
	tests := []struct {
		name    string
		entries []PlanEntry
		wantErr bool
	}{
		{name: "a single valid entry", entries: ok("write the parser")},
		{name: "all three statuses and priorities", entries: []PlanEntry{
			entry("a", "one", PlanEntryPending, PlanEntryPriorityHigh),
			entry("b", "two", PlanEntryInProgress, PlanEntryPriorityMedium),
			entry("c", "three", PlanEntryCompleted, PlanEntryPriorityLow),
		}},
		{name: "empty snapshot is rejected", entries: []PlanEntry{}, wantErr: true},
		{name: "empty content is rejected", entries: []PlanEntry{entry("e1", "", PlanEntryPending, PlanEntryPriorityLow)}, wantErr: true},
		{name: "unknown status is rejected", entries: []PlanEntry{entry("e1", "ok", "bogus", PlanEntryPriorityLow)}, wantErr: true},
		{name: "empty status is rejected", entries: []PlanEntry{entry("e1", "ok", "", PlanEntryPriorityLow)}, wantErr: true},
		{name: "unknown priority is rejected", entries: []PlanEntry{entry("e1", "ok", PlanEntryPending, "urgent")}, wantErr: true},
		{name: "empty priority is rejected", entries: []PlanEntry{entry("e1", "ok", PlanEntryPending, "")}, wantErr: true},
		{name: "too many entries is rejected", entries: tooManyEntries(maxPlanEntries + 1), wantErr: true},
		{name: "entry at the count cap is accepted", entries: tooManyEntries(maxPlanEntries)},
		{name: "oversized content is rejected", entries: ok(strings.Repeat("x", maxPlanEntryBytes+1)), wantErr: true},
		{name: "stream-leak garbage is rejected", entries: ok("self.__next_f.push([1, \"chunk\"])"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlan(tt.entries)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func tooManyEntries(n int) []PlanEntry {
	out := make([]PlanEntry, n)
	for i := range out {
		out[i] = entry(fmt.Sprintf("e%d", i), fmt.Sprintf("step %d", i), PlanEntryPending, PlanEntryPriorityLow)
	}
	return out
}

// planContentLooksCorrupted flags a heavy backslash density only past the
// minimum length, so a short escaped string is fine.
func TestUnit_PlanContentCorruptionHeuristic(t *testing.T) {
	require.False(t, planContentLooksCorrupted(`a\b\c`), "a short escaped string is not corruption")
	heavy := strings.Repeat(`\`, planEscapeMinLen)
	require.True(t, planContentLooksCorrupted(heavy), "a long, mostly-backslash blob is a stream leak")
}

// ─── SetPlan: full-snapshot replace ─────────────────────────────────────────

// The first SetPlan assigns ids to entries lacking one, sets revision 1, stores
// the explanation, and persists — and returns the stored snapshot so a caller
// projects exactly what was written.
func TestUnit_MissionService_SetPlanFirstRevisionAssignsIDsAndPersists(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("plan this")
	require.NoError(t, svc.Create(ctx, m))

	planned, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("", "scope the work", PlanEntryInProgress, PlanEntryPriorityHigh),
		entry("", "write the code", PlanEntryPending, PlanEntryPriorityMedium),
	}, "initial plan")
	require.NoError(t, err)
	require.Equal(t, 1, planned.Plan.Revision)
	require.Equal(t, "initial plan", planned.Plan.Explanation)
	require.Len(t, planned.Plan.Entries, 2)
	require.NotEmpty(t, planned.Plan.Entries[0].ID, "an entry with no id gets one assigned")
	require.NotEmpty(t, planned.Plan.Entries[1].ID)
	require.NotEqual(t, planned.Plan.Entries[0].ID, planned.Plan.Entries[1].ID)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 1, persisted.Plan.Revision)
	require.Equal(t, "scope the work", persisted.Plan.Entries[0].Content)
}

// SetPlan is full-snapshot replace: the second call's list wholly replaces the
// first, deletion is omission, and the revision counter climbs by one.
func TestUnit_MissionService_SetPlanReplacesWholeSnapshotAndBumpsRevision(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("replace the plan")
	require.NoError(t, svc.Create(ctx, m))

	r1, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("keep", "step to keep", PlanEntryPending, PlanEntryPriorityMedium),
		entry("drop", "step to drop", PlanEntryPending, PlanEntryPriorityLow),
	}, "first")
	require.NoError(t, err)
	require.Equal(t, 1, r1.Plan.Revision)

	r2, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("keep", "step to keep", PlanEntryInProgress, PlanEntryPriorityMedium),
		entry("new", "a brand new step", PlanEntryPending, PlanEntryPriorityHigh),
	}, "second")
	require.NoError(t, err)
	require.Equal(t, 2, r2.Plan.Revision)
	require.Equal(t, "second", r2.Plan.Explanation)
	require.Len(t, r2.Plan.Entries, 2, "the whole list is replaced")

	ids := map[string]bool{}
	for _, e := range r2.Plan.Entries {
		ids[e.ID] = true
	}
	require.True(t, ids["keep"])
	require.True(t, ids["new"])
	require.False(t, ids["drop"], "an omitted entry is deleted from the snapshot")
}

// The audit-safety guard (blueprint pattern 5): a snapshot may not rewrite the
// content of an entry that was completed in the prior revision.
func TestUnit_MissionService_SetPlanRejectsRewritingCompletedEntry(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("immutable done work")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("done", "shipped the parser", PlanEntryCompleted, PlanEntryPriorityHigh),
		entry("next", "write the tests", PlanEntryInProgress, PlanEntryPriorityMedium),
	}, "rev 1")
	require.NoError(t, err)

	// Rewriting the completed entry's content (same id) is rejected.
	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("done", "shipped the parser AND the lexer", PlanEntryCompleted, PlanEntryPriorityHigh),
		entry("next", "write the tests", PlanEntryInProgress, PlanEntryPriorityMedium),
	}, "rev 2 rewrites done")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already-completed")

	// The prior revision survives the rejected write.
	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 1, persisted.Plan.Revision)
	require.Equal(t, "shipped the parser", persisted.Plan.Entries[0].Content)
}

// A correction to completed work is allowed as a NEW entry (a new id), and the
// completed entry may keep its exact content while everything else changes.
func TestUnit_MissionService_SetPlanAllowsAppendingCorrectionOfCompletedWork(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("append corrections")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("done", "shipped the parser", PlanEntryCompleted, PlanEntryPriorityHigh),
	}, "rev 1")
	require.NoError(t, err)

	r2, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("done", "shipped the parser", PlanEntryCompleted, PlanEntryPriorityHigh),
		entry("fix", "the parser missed a case; patch it", PlanEntryInProgress, PlanEntryPriorityHigh),
	}, "rev 2 appends a correction")
	require.NoError(t, err, "an unchanged completed entry plus a new correction entry is allowed")
	require.Equal(t, 2, r2.Plan.Revision)
	require.Len(t, r2.Plan.Entries, 2)
}

// Discipline is NOT host-enforced (blueprint pattern 3): a snapshot with more
// than one in_progress entry is accepted — that rule lives in the planner prompt.
func TestUnit_MissionService_SetPlanDoesNotEnforceOneInProgress(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("soft discipline")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryInProgress, PlanEntryPriorityHigh),
		entry("b", "step b", PlanEntryInProgress, PlanEntryPriorityMedium),
	}, "two in progress on purpose")
	require.NoError(t, err, "host validation is soft on discipline; two in_progress is allowed")
}

func TestUnit_MissionService_SetPlanRejectsInvalidSnapshot(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("bad plan")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, nil, "x")
	require.Error(t, err, "an empty snapshot is rejected")

	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{entry("e", "   ", PlanEntryPending, PlanEntryPriorityLow)}, "x")
	require.Error(t, err, "whitespace-only content is rejected")

	// A rejected SetPlan leaves the mission unplanned.
	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 0, persisted.Plan.Revision)
}

func TestUnit_MissionService_SetPlanUnknownMissionReturnsNotFound(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	_, err := svc.SetPlan(ctx, "no-such-id", []PlanEntry{entry("e", "step", PlanEntryPending, PlanEntryPriorityLow)}, "x")
	require.Error(t, err)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

// A pristine mission carries the zero Plan: revision 0, no entries. This is also
// exactly what a legacy row written before the plan field existed decodes to.
func TestUnit_MissionService_FreshMissionHasZeroPlan(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("never planned")
	require.NoError(t, svc.Create(ctx, m))

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 0, got.Plan.Revision)
	require.Empty(t, got.Plan.Entries)
}

// ─── plan_revised events ────────────────────────────────────────────────────

// A successful SetPlan publishes a self-contained PlanRevisedEvent carrying the
// supervision edge, the revision/explanation, and the counts the inbox renders.
func TestUnit_MissionService_SetPlanPublishesPlanRevisedEvent(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("supervised plan")
	m.ParentSessionID = "parent-session-3"
	require.NoError(t, svc.Create(ctx, m))

	// Revision 1: two entries, both new.
	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryCompleted, PlanEntryPriorityHigh),
		entry("b", "step b", PlanEntryInProgress, PlanEntryPriorityMedium),
	}, "first plan")
	require.NoError(t, err)

	// Revision 2: keep a and b, add c, drop nothing — one added.
	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryCompleted, PlanEntryPriorityHigh),
		entry("b", "step b", PlanEntryCompleted, PlanEntryPriorityMedium),
		entry("c", "step c", PlanEntryPending, PlanEntryPriorityLow),
	}, "added a step")
	require.NoError(t, err)

	evs := pub.planRevisedEvents(t)
	require.Len(t, evs, 2)

	first := evs[0]
	require.Equal(t, m.ID, first.MissionID)
	require.Equal(t, "parent-session-3", first.ParentSessionID, "the supervision edge rides on the event")
	require.Equal(t, m.AgentName, first.AgentName)
	require.Equal(t, m.Intent, first.Intent)
	require.Equal(t, 1, first.Revision)
	require.Equal(t, "first plan", first.Explanation)
	require.Equal(t, 2, first.EntryCount)
	require.Equal(t, 2, first.Added, "both entries are new in revision 1")
	require.Equal(t, 0, first.Removed)
	require.Equal(t, 1, first.Completed)
	require.Equal(t, 1, first.InProgress)

	second := evs[1]
	require.Equal(t, 2, second.Revision)
	require.Equal(t, "added a step", second.Explanation)
	require.Equal(t, 3, second.EntryCount)
	require.Equal(t, 1, second.Added, "only c is new in revision 2")
	require.Equal(t, 0, second.Removed)
	require.Equal(t, 2, second.Completed)
	require.Equal(t, 1, second.Pending)
}

// The removed delta counts an id that vanished from the snapshot.
func TestUnit_MissionService_PlanRevisedEventCountsRemoved(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("dropped a step")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryPending, PlanEntryPriorityHigh),
		entry("b", "step b", PlanEntryPending, PlanEntryPriorityLow),
	}, "two steps")
	require.NoError(t, err)
	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryPending, PlanEntryPriorityHigh),
	}, "dropped b")
	require.NoError(t, err)

	evs := pub.planRevisedEvents(t)
	require.Len(t, evs, 2)
	require.Equal(t, 1, evs[1].Removed, "b vanished from the snapshot")
	require.Equal(t, 0, evs[1].Added)
}

// The best-effort invariant: a publish failure never turns a durably-stored plan
// revision into a failed SetPlan.
func TestUnit_MissionService_SetPlanPublishFailureDoesNotFailSetPlan(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{err: fmt.Errorf("bus is down")}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("survive a broken bus")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{entry("a", "step", PlanEntryPending, PlanEntryPriorityLow)}, "x")
	require.NoError(t, err, "a publish failure must not fail SetPlan — the plan is the durable fact")

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 1, persisted.Plan.Revision, "the plan is durable regardless of the routing nudge")
}

// The publisher is optional: a service built without one stores plans and simply
// publishes nothing.
func TestUnit_MissionService_SetPlanNoPublisherStillStores(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db) // no publisher

	m := newMission("no bus wired")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{entry("a", "step", PlanEntryPending, PlanEntryPriorityLow)}, "x")
	require.NoError(t, err)

	persisted, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Equal(t, 1, persisted.Plan.Revision)
}

// ─── plan-revision history ring (Mission.PlanRevisions) ─────────────────────

// Every successful SetPlan appends one summary to the durable ring, in
// chronological (oldest-first) order, and the ring is present on the mission GET
// even with NO bus wired — the history is a durable fact, not a routing nudge.
func TestUnit_MissionService_PlanRevisionsAccrueOnGet(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db) // deliberately no publisher: the ring must not depend on the bus

	m := newMission("skimmable history")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryPending, PlanEntryPriorityHigh),
	}, "first")
	require.NoError(t, err)
	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryInProgress, PlanEntryPriorityHigh),
		entry("b", "step b", PlanEntryPending, PlanEntryPriorityMedium),
	}, "second")
	require.NoError(t, err)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Len(t, got.PlanRevisions, 2, "one summary per SetPlan, durable without a bus")

	require.Equal(t, 1, got.PlanRevisions[0].Revision, "oldest-first ordering")
	require.Equal(t, "first", got.PlanRevisions[0].Explanation)
	require.Equal(t, 1, got.PlanRevisions[0].Added)
	require.Equal(t, 1, got.PlanRevisions[0].Pending)
	require.False(t, got.PlanRevisions[0].At.IsZero(), "each summary is stamped with its revision time")

	require.Equal(t, 2, got.PlanRevisions[1].Revision)
	require.Equal(t, "second", got.PlanRevisions[1].Explanation)
	require.Equal(t, 1, got.PlanRevisions[1].Added, "only b is new in revision 2")
	require.Equal(t, 1, got.PlanRevisions[1].InProgress)
	require.Equal(t, 1, got.PlanRevisions[1].Pending)
}

// The ring is bounded at maxPlanRevisions: past that, the oldest summaries drop
// and only the last N survive, so a heavily-replanned mission cannot grow its KV
// row without limit.
func TestUnit_MissionService_PlanRevisionsRingIsBounded(t *testing.T) {
	ctx, db := setupMissionDB(t)
	svc := New(db)

	m := newMission("replan a lot")
	require.NoError(t, svc.Create(ctx, m))

	total := maxPlanRevisions + 5
	for i := 1; i <= total; i++ {
		_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
			entry("a", fmt.Sprintf("step a rev %d", i), PlanEntryPending, PlanEntryPriorityLow),
		}, fmt.Sprintf("rev %d", i))
		require.NoError(t, err)
	}

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	require.Len(t, got.PlanRevisions, maxPlanRevisions, "the ring is capped at the last N")
	// The oldest kept is revision total-N+1; the newest is total. Ordering holds.
	require.Equal(t, total-maxPlanRevisions+1, got.PlanRevisions[0].Revision, "the oldest kept summary")
	require.Equal(t, total, got.PlanRevisions[maxPlanRevisions-1].Revision, "the newest summary is last")
	require.Equal(t, total, got.Plan.Revision, "the live revision counter keeps climbing past the ring")
}

// The stored summaries carry the SAME numbers as the published PlanRevisedEvent
// for the same revision — they are built from one value in SetPlan, so history
// and nudge can never drift.
func TestUnit_MissionService_PlanRevisionsMatchPublishedEvents(t *testing.T) {
	ctx, db := setupMissionDB(t)
	pub := &fakePublisher{}
	svc := New(db, WithEventPublisher(pub))

	m := newMission("history matches events")
	require.NoError(t, svc.Create(ctx, m))

	_, err := svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("a", "step a", PlanEntryCompleted, PlanEntryPriorityHigh),
		entry("b", "step b", PlanEntryInProgress, PlanEntryPriorityMedium),
	}, "first plan")
	require.NoError(t, err)
	_, err = svc.SetPlan(ctx, m.ID, []PlanEntry{
		entry("b", "step b", PlanEntryCompleted, PlanEntryPriorityMedium),
		entry("c", "step c", PlanEntryPending, PlanEntryPriorityLow),
	}, "dropped a, added c")
	require.NoError(t, err)

	got, err := svc.Get(ctx, m.ID)
	require.NoError(t, err)
	evs := pub.planRevisedEvents(t)
	require.Len(t, got.PlanRevisions, 2)
	require.Len(t, evs, 2)

	for i := range evs {
		sum := got.PlanRevisions[i]
		ev := evs[i]
		require.Equal(t, ev.Revision, sum.Revision, "revision matches")
		require.Equal(t, ev.Explanation, sum.Explanation, "explanation matches")
		require.Equal(t, ev.Added, sum.Added, "added matches")
		require.Equal(t, ev.Removed, sum.Removed, "removed matches")
		require.Equal(t, ev.Pending, sum.Pending, "pending matches")
		require.Equal(t, ev.InProgress, sum.InProgress, "inProgress matches")
		require.Equal(t, ev.Completed, sum.Completed, "completed matches")
	}
}

// A legacy mission row written before the PlanRevisions field existed carries no
// `planRevisions` key; it must decode to a nil ring with everything else intact,
// exactly like the zero-Plan legacy story. omitempty guarantees the field is
// absent (not `null`) on any never-revised mission written today, too.
func TestUnit_MissionService_LegacyMissionDecodesToNilRing(t *testing.T) {
	legacy := `{
		"id": "legacy-1",
		"intent": "predates the ring",
		"agentName": "runner",
		"hitlPolicyName": "hitl-policy-default.json",
		"status": "open",
		"plan": {"entries": [{"id":"e","content":"do it","status":"pending","priority":"low"}], "revision": 3},
		"createdAt": "2026-07-01T00:00:00Z",
		"updatedAt": "2026-07-01T00:00:00Z"
	}`
	var m Mission
	require.NoError(t, json.Unmarshal([]byte(legacy), &m))
	require.Nil(t, m.PlanRevisions, "a row with no planRevisions key decodes to a nil ring")
	require.Equal(t, 3, m.Plan.Revision, "the rest of the record still parses")
	require.Len(t, m.Plan.Entries, 1)

	// And a freshly-planned mission never revised through SetPlan still marshals
	// without a planRevisions key (omitempty), so it round-trips as a legacy row.
	fresh := Mission{ID: "fresh", Intent: "x"}
	raw, err := json.Marshal(fresh)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "planRevisions", "a nil ring is omitted from the wire, not sent as null")
}
