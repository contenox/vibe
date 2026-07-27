package operatorinbox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupInboxDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "operatorinbox.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// TestUnit_Inbox_AddAndListRoundtrip stores an item and reads it back, asserting
// the self-contained snapshot (mission attribution + embedded report + reason)
// survives the roundtrip and that an id/timestamp are assigned when absent.
func TestUnit_Inbox_AddAndListRoundtrip(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	item := &Item{
		MissionID: "m1",
		AgentName: "runner",
		Intent:    "do the thing",
		Reason:    ReasonOperatorFired,
		Report:    missionservice.Report{ID: "r1", MissionID: "m1", Kind: missionservice.ReportKindResult, Summary: "done"},
	}
	require.NoError(t, svc.Add(ctx, item))
	require.NotEmpty(t, item.ID, "Add assigns an id when absent")
	require.False(t, item.CreatedAt.IsZero(), "Add stamps CreatedAt when absent")

	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	got := items[0]
	require.Equal(t, item.ID, got.ID)
	require.Equal(t, "m1", got.MissionID)
	require.Equal(t, "runner", got.AgentName)
	require.Equal(t, ReasonOperatorFired, got.Reason)
	require.Equal(t, "done", got.Report.Summary)
	require.Equal(t, missionservice.ReportKindResult, got.Report.Kind)
}

// TestUnit_Inbox_ListNewestFirst asserts the list ordering an operator reads.
func TestUnit_Inbox_ListNewestFirst(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	require.NoError(t, svc.Add(ctx, &Item{MissionID: "m1", Reason: ReasonOperatorFired,
		Report: missionservice.Report{Kind: missionservice.ReportKindProgress, Summary: "first"}}))
	require.NoError(t, svc.Add(ctx, &Item{MissionID: "m2", Reason: ReasonParentGone, ParentSessionID: "gone",
		Report: missionservice.Report{Kind: missionservice.ReportKindResult, Summary: "second"}}))

	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "second", items[0].Report.Summary, "newest first")
	require.Equal(t, "first", items[1].Report.Summary)
	require.Equal(t, ReasonParentGone, items[0].Reason)
	require.Equal(t, "gone", items[0].ParentSessionID)
}

// TestUnit_Inbox_EmptyIsNonNil proves an empty inbox renders as [], not null.
func TestUnit_Inbox_EmptyIsNonNil(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)
	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, items)
	require.Empty(t, items)
}

// TestUnit_Inbox_AddValidates rejects the two ways an item is unusable: no
// mission to attribute it to, and an unknown reason.
func TestUnit_Inbox_AddValidates(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	require.Error(t, svc.Add(ctx, &Item{Reason: ReasonOperatorFired}), "missionId is required")
	require.Error(t, svc.Add(ctx, &Item{MissionID: "m1", Reason: "bogus"}), "reason must be a known value")
	require.Error(t, svc.Add(ctx, nil))
}

func landedItem(missionID, summary string) *Item {
	return &Item{
		MissionID: missionID,
		AgentName: "runner",
		Reason:    ReasonOperatorFired,
		Report: missionservice.Report{
			ID: "r-" + missionID, MissionID: missionID,
			Kind: missionservice.ReportKindResult, Summary: summary,
		},
	}
}

// TestUnit_Inbox_AddPublishesTheStoredItem is the live-surface signal: a
// successful Add announces the item on AddedSubject so a watcher learns of it
// without polling. Driven over the SQLite bus — the backend the fleet actually
// runs on, and the one that carries a cross-process event — rather than the
// in-memory one, so the payload is proven to survive a real round trip.
func TestUnit_Inbox_AddPublishesTheStoredItem(t *testing.T) {
	ctx, db := setupInboxDB(t)
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{
		EventPoll: time.Millisecond,
	})
	t.Cleanup(func() { _ = bus.Close() })

	ch := make(chan []byte, 4)
	sub, err := bus.Stream(ctx, AddedSubject, ch)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	svc := New(db, WithEventPublisher(bus))
	item := landedItem("m1", "shipped the board")
	require.NoError(t, svc.Add(ctx, item))

	var raw []byte
	select {
	case raw = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("Add must announce the stored item on AddedSubject")
	}

	var got Item
	require.NoError(t, json.Unmarshal(raw, &got), "the payload is the JSON of the stored Item")
	require.Equal(t, item.ID, got.ID)
	require.Equal(t, "m1", got.MissionID)
	require.Equal(t, "runner", got.AgentName)
	require.Equal(t, ReasonOperatorFired, got.Reason)
	require.Equal(t, "shipped the board", got.Report.Summary)
	require.False(t, got.Acked, "a freshly landed item is unacked")

	// The announced payload and the stored row are the same bytes: a subscriber
	// that renders from the event sees exactly what a reader lists.
	stored, err := svc.Get(ctx, item.ID)
	require.NoError(t, err)
	require.Equal(t, stored.ID, got.ID)
	require.Equal(t, stored.CreatedAt.UTC(), got.CreatedAt.UTC())
}

type failingPublisher struct {
	mu    sync.Mutex
	calls int
}

func (p *failingPublisher) Publish(context.Context, string, []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return fmt.Errorf("bus down")
}

// TestUnit_Inbox_PublishFailureNeverFailsAdd holds the best-effort invariant one
// layer down from the router: the item is the durable fact, the announcement is
// a nudge on top of it. A report that reached no live supervisor must not be
// lost a second time because the bus was down.
func TestUnit_Inbox_PublishFailureNeverFailsAdd(t *testing.T) {
	ctx, db := setupInboxDB(t)
	pub := &failingPublisher{}
	svc := New(db, WithEventPublisher(pub))

	item := landedItem("m1", "still durable")
	require.NoError(t, svc.Add(ctx, item), "a failed publish must never fail the Add")
	require.Equal(t, 1, pub.calls)

	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "still durable", items[0].Report.Summary)
}

// recordingTracker records the (operation, subject, error) of every report so a
// test can assert what the best-effort publish path said when it shrugged.
type recordingTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	op, subject string
	err         error
}

func (r *recordingTracker) Start(_ context.Context, op, subject string, _ ...any) (func(error), func(string, any), func()) {
	return func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, trackedEvent{op: op, subject: subject, err: err})
		},
		func(string, any) {},
		func() {}
}

func (r *recordingTracker) errorsFor(op, subject string) []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []error
	for _, ev := range r.events {
		if ev.op == op && ev.subject == subject && ev.err != nil {
			out = append(out, ev.err)
		}
	}
	return out
}

var _ libtracker.ActivityTracker = (*recordingTracker)(nil)

// TestUnit_Inbox_PublishFailureIsReportedToTracker proves the shrug is audible:
// swallowing the bus error entirely would leave an operator with a live nudge
// that silently never arrives, so the failure is reported through the tracker
// even though it never reaches Add's caller.
func TestUnit_Inbox_PublishFailureIsReportedToTracker(t *testing.T) {
	ctx, db := setupInboxDB(t)
	tracker := &recordingTracker{}
	svc := New(db, WithEventPublisher(&failingPublisher{}), WithTracker(tracker))

	require.NoError(t, svc.Add(ctx, landedItem("m1", "still durable")))

	reported := tracker.errorsFor("publish", "inbox_item_added_event")
	require.Len(t, reported, 1, "a failed publish is reported exactly once")
	require.Contains(t, reported[0].Error(), "bus down", "the report carries the bus error")
	require.Contains(t, reported[0].Error(), "live nudge skipped",
		"the report keeps the consequence: stored, not announced")
}

// TestUnit_Inbox_PublishSuccessReportsNothing pins the volume: the tracker hears
// from this path only when the publish fails.
func TestUnit_Inbox_PublishSuccessReportsNothing(t *testing.T) {
	ctx, db := setupInboxDB(t)
	tracker := &recordingTracker{}
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{
		EventPoll: time.Millisecond,
	})
	t.Cleanup(func() { _ = bus.Close() })
	svc := New(db, WithEventPublisher(bus), WithTracker(tracker))

	require.NoError(t, svc.Add(ctx, landedItem("m1", "quiet success")))
	require.Empty(t, tracker.errorsFor("publish", "inbox_item_added_event"))
}

// TestUnit_Inbox_NoPublisherStillAdds proves the seam is optional: an inbox built
// the way every caller before this existed built it stores and lists unchanged.
func TestUnit_Inbox_NoPublisherStillAdds(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)
	require.NoError(t, svc.Add(ctx, landedItem("m1", "no bus wired")))
	items, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

// TestUnit_Inbox_GetRoundtrip reads one item back by id — the CLI's `inbox show`
// path — and pins the typed miss for an unknown id.
func TestUnit_Inbox_GetRoundtrip(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	item := landedItem("m1", "found me")
	require.NoError(t, svc.Add(ctx, item))

	got, err := svc.Get(ctx, item.ID)
	require.NoError(t, err)
	require.Equal(t, item.ID, got.ID)
	require.Equal(t, "found me", got.Report.Summary)
	require.Equal(t, ReasonOperatorFired, got.Reason)

	_, err = svc.Get(ctx, "nope")
	require.ErrorIs(t, err, ErrNotFound, "an unknown id is a typed miss")
	require.ErrorIs(t, err, libdb.ErrNotFound, "and satisfies the codebase-wide store miss too")

	_, err = svc.Get(ctx, "")
	require.Error(t, err, "an empty id is a caller bug, not a miss")
}

// TestUnit_Inbox_AckMarksSeenAndKeepsTheRecord: Ack is a read mark, not a
// delete. The item stays listed by List (the full record of what reached no live
// supervisor) and drops out of ListUnacked (the worklist).
func TestUnit_Inbox_AckMarksSeenAndKeepsTheRecord(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	item := landedItem("m1", "read me")
	require.NoError(t, svc.Add(ctx, item))

	before := time.Now().UTC().Add(-time.Second)
	require.NoError(t, svc.Ack(ctx, item.ID))

	got, err := svc.Get(ctx, item.ID)
	require.NoError(t, err, "acking must not erase the record that a report reached no supervisor")
	require.True(t, got.Acked)
	require.NotNil(t, got.AckedAt)
	require.True(t, got.AckedAt.After(before))

	all, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, all, 1, "List is the full record and still shows an acked item")

	unacked, err := svc.ListUnacked(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, unacked)
	require.Empty(t, unacked, "the worklist is empty once everything is read")
}

// TestUnit_Inbox_AckIsIdempotentAndTyped: a retried CLI ack is a success, and an
// unknown id is the same typed miss Get gives.
func TestUnit_Inbox_AckIsIdempotentAndTyped(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	item := landedItem("m1", "twice")
	require.NoError(t, svc.Add(ctx, item))
	require.NoError(t, svc.Ack(ctx, item.ID))

	first, err := svc.Get(ctx, item.ID)
	require.NoError(t, err)

	require.NoError(t, svc.Ack(ctx, item.ID), "acking twice is a no-op success")
	second, err := svc.Get(ctx, item.ID)
	require.NoError(t, err)
	require.Equal(t, first.AckedAt.UTC(), second.AckedAt.UTC(), "the first read time stands")

	require.ErrorIs(t, svc.Ack(ctx, "nope"), ErrNotFound)
	require.Error(t, svc.Ack(ctx, ""), "an empty id is a caller bug, not a miss")
}

// TestUnit_Inbox_ListUnackedKeepsOrderAndFilters is the worklist view an
// operator works down: newest-first, acked items gone, unacked ones kept.
func TestUnit_Inbox_ListUnackedKeepsOrderAndFilters(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	first := landedItem("m1", "first")
	require.NoError(t, svc.Add(ctx, first))
	second := landedItem("m2", "second")
	require.NoError(t, svc.Add(ctx, second))
	third := landedItem("m3", "third")
	require.NoError(t, svc.Add(ctx, third))

	require.NoError(t, svc.Ack(ctx, second.ID))

	unacked, err := svc.ListUnacked(ctx, 100)
	require.NoError(t, err)
	require.Len(t, unacked, 2)
	require.Equal(t, "third", unacked[0].Report.Summary, "newest first, same as List")
	require.Equal(t, "first", unacked[1].Report.Summary)

	all, err := svc.List(ctx, 100)
	require.NoError(t, err)
	require.Len(t, all, 3)
}

// TestUnit_Inbox_ListUnackedFillsItsLimit is the reason ListUnacked pages rather
// than filtering one page: with a worked-through backlog in front of them, a
// caller asking for N unacked items must still get N, not "whatever survived the
// first N rows."
func TestUnit_Inbox_ListUnackedFillsItsLimit(t *testing.T) {
	ctx, db := setupInboxDB(t)
	svc := New(db)

	var unackedIDs []string
	for i := range 12 {
		it := landedItem(fmt.Sprintf("m%02d", i), fmt.Sprintf("item %02d", i))
		require.NoError(t, svc.Add(ctx, it))
		// Ack everything but the three oldest, so a naive "read `limit` rows then
		// filter" returns nothing for limit<=9.
		if i >= 3 {
			require.NoError(t, svc.Ack(ctx, it.ID))
		} else {
			unackedIDs = append(unackedIDs, it.ID)
		}
	}

	unacked, err := svc.ListUnacked(ctx, 3)
	require.NoError(t, err)
	require.Len(t, unacked, 3, "the acked backlog in front must not starve the worklist")
	got := map[string]bool{}
	for _, it := range unacked {
		got[it.ID] = true
	}
	for _, id := range unackedIDs {
		require.True(t, got[id], "every unacked item is reachable")
	}
}
