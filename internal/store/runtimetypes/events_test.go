package runtimetypes_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

const testEventWS = "ws-test"

func setupEventDB(t *testing.T) libdb.DBManager {
	t.Helper()
	_, db := runtimetypes.SetupDBManager(t)
	return db
}

type movableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *movableClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func errFromList[T any](_ T, err error) error { return err }

func TestUnit_Events_AppendAssignsOrderedNIDsAndDefaults(t *testing.T) {
	ctx := context.Background()
	store := runtimetypes.NewEventStore(setupEventDB(t))

	first := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.a", Source: "test"}
	require.NoError(t, store.AppendEvent(ctx, first))
	second := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.b", Data: json.RawMessage(`{"k":"v"}`)}
	require.NoError(t, store.AppendEvent(ctx, second))

	require.Greater(t, first.NID, int64(0))
	require.Equal(t, first.NID+1, second.NID, "NIDs must be assigned in append order")
	require.False(t, first.Time.IsZero(), "zero Time defaults to now")
	require.JSONEq(t, `{}`, string(first.Data), "nil Data defaults to {}")
	require.Equal(t, 0, first.Hop)

	require.ErrorIs(t, store.AppendEvent(ctx, &runtimetypes.Event{}), runtimetypes.ErrEventTypeRequired)
}

func TestUnit_Events_AppendTakesHopFromContext(t *testing.T) {
	ctx := context.Background()
	store := runtimetypes.NewEventStore(setupEventDB(t))

	inherited := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.hop"}
	require.NoError(t, store.AppendEvent(runtimetypes.WithEventHop(ctx, 3), inherited))
	require.Equal(t, 3, inherited.Hop, "zero hop inherits the context hop")

	explicit := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.hop", Hop: 1}
	require.NoError(t, store.AppendEvent(runtimetypes.WithEventHop(ctx, 3), explicit))
	require.Equal(t, 1, explicit.Hop, "an explicit hop wins over the context")

	got, err := store.ListEventsSince(ctx, testEventWS, 0, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 3, got[0].Hop)
	require.Equal(t, 1, got[1].Hop)
}

func TestUnit_Events_ListEventsSinceCursor(t *testing.T) {
	ctx := context.Background()
	store := runtimetypes.NewEventStore(setupEventDB(t))

	var nids []int64
	for range 5 {
		ev := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.seq"}
		require.NoError(t, store.AppendEvent(ctx, ev))
		nids = append(nids, ev.NID)
	}

	all, err := store.ListEventsSince(ctx, testEventWS, 0, 100)
	require.NoError(t, err)
	require.Len(t, all, 5)
	for i, ev := range all {
		require.Equal(t, nids[i], ev.NID, "ascending nid order")
	}

	tail, err := store.ListEventsSince(ctx, testEventWS, nids[2], 100)
	require.NoError(t, err)
	require.Len(t, tail, 2, "cursor reads are strictly after afterNID")
	require.Equal(t, nids[3], tail[0].NID)

	page, err := store.ListEventsSince(ctx, testEventWS, 0, 2)
	require.NoError(t, err)
	require.Len(t, page, 2, "limit bounds one page")

	empty, err := store.ListEventsSince(ctx, testEventWS, nids[4], 100)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestUnit_Events_AppendCreatesPartitionsAcrossPeriodBoundary(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{now: time.Date(2026, 8, 1, 23, 55, 0, 0, time.UTC)}
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	day1 := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.boundary"}
	require.NoError(t, store.AppendEvent(ctx, day1))

	clock.Set(time.Date(2026, 8, 2, 0, 5, 0, 0, time.UTC))
	day2 := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.boundary"}
	require.NoError(t, store.AppendEvent(ctx, day2))

	parts, err := store.ListEventPartitions(ctx)
	require.NoError(t, err)
	require.Len(t, parts, 2, "one partition per UTC day, created on demand")
	require.Equal(t, "20260801", parts[0].Period)
	require.Equal(t, "event_log_20260801", parts[0].TableName)
	require.Equal(t, "20260802", parts[1].Period)

	require.Equal(t, day1.NID+1, day2.NID, "the global sequence stays monotonic across partitions")
}

func TestUnit_Events_ListEventsSinceSpansPartitionsInNIDOrder(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{now: time.Date(2026, 8, 1, 23, 55, 0, 0, time.UTC)}
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	// e3's Time falls back into day 1 despite being appended after the clock moved to day 2.
	e1 := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.span"}
	require.NoError(t, store.AppendEvent(ctx, e1))
	clock.Set(time.Date(2026, 8, 2, 0, 5, 0, 0, time.UTC))
	e2 := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.span"}
	require.NoError(t, store.AppendEvent(ctx, e2))
	e3 := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.span", Time: time.Date(2026, 8, 1, 23, 59, 0, 0, time.UTC)}
	require.NoError(t, store.AppendEvent(ctx, e3))

	all, err := store.ListEventsSince(ctx, testEventWS, 0, 100)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, []int64{e1.NID, e2.NID, e3.NID}, []int64{all[0].NID, all[1].NID, all[2].NID},
		"ascending NID across partitions, not period order")

	tail, err := store.ListEventsSince(ctx, testEventWS, e1.NID, 100)
	require.NoError(t, err)
	require.Len(t, tail, 2)
	require.Equal(t, e2.NID, tail[0].NID)

	page, err := store.ListEventsSince(ctx, testEventWS, 0, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, []int64{e1.NID, e2.NID}, []int64{page[0].NID, page[1].NID},
		"limit truncation keeps NID order so a cursor never skips an event")
}

func TestUnit_Events_AppendRejectsTimesOutsideAcceptanceWindow(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(func() time.Time { return now }))

	tooOld := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.window", Time: now.Add(-runtimetypes.EventAcceptanceWindow - time.Second)}
	require.ErrorIs(t, store.AppendEvent(ctx, tooOld), runtimetypes.ErrEventTooOld)

	tooNew := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.window", Time: now.Add(runtimetypes.EventAcceptanceWindow + time.Second)}
	require.ErrorIs(t, store.AppendEvent(ctx, tooNew), runtimetypes.ErrEventTooNew)

	edge := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.window", Time: now.Add(-runtimetypes.EventAcceptanceWindow + time.Second)}
	require.NoError(t, store.AppendEvent(ctx, edge))
}

func TestUnit_Events_PruneDropsOldPeriodsAndKeepsNewer(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{now: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	old := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.prune"}
	require.NoError(t, store.AppendEvent(ctx, old))
	clock.Set(time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC))
	kept := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.prune"}
	require.NoError(t, store.AppendEvent(ctx, kept))

	dropped, err := store.PruneEventPartitionsBefore(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, []string{"20260730"}, dropped)

	parts, err := store.ListEventPartitions(ctx)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, "20260802", parts[0].Period)

	remaining, err := store.ListEventsSince(ctx, testEventWS, 0, 100)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "newer events survive the prune")
	require.Equal(t, kept.NID, remaining[0].NID)

	fresh := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.prune"}
	require.NoError(t, store.AppendEvent(ctx, fresh))
	require.Equal(t, kept.NID+1, fresh.NID, "the NID sequence is untouched by pruning")

	again, err := store.PruneEventPartitionsBefore(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Empty(t, again, "pruning is idempotent")
}

// TestUnit_Events_ConcurrentAppendsAcrossPeriodBoundary asserts racing appenders across a period boundary converge on one partition per period and never share an NID.
func TestUnit_Events_ConcurrentAppendsAcrossPeriodBoundary(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{now: time.Date(2026, 8, 1, 23, 59, 0, 0, time.UTC)}
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	const appenders = 12
	nids := make([]int64, appenders)
	errs := make([]error, appenders)
	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Both times sit inside the acceptance window, so half the appenders race on each period's creation.
			ts := time.Date(2026, 8, 1, 23, 59, 30, 0, time.UTC)
			if i%2 == 1 {
				ts = time.Date(2026, 8, 2, 0, 0, 30, 0, time.UTC)
			}
			ev := &runtimetypes.Event{WorkspaceID: testEventWS, Type: "test.events.race", Time: ts}
			errs[i] = store.AppendEvent(ctx, ev)
			nids[i] = ev.NID
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "appender %d", i)
	}

	parts, err := store.ListEventPartitions(ctx)
	require.NoError(t, err)
	require.Len(t, parts, 2, "racing appenders create one partition per period, not one each")
	require.Equal(t, "20260801", parts[0].Period)
	require.Equal(t, "20260802", parts[1].Period)

	seen := map[int64]bool{}
	for _, nid := range nids {
		require.False(t, seen[nid], "NID %d handed out twice", nid)
		seen[nid] = true
	}
	require.Len(t, seen, appenders)

	all, err := store.ListEventsSince(ctx, testEventWS, 0, 100)
	require.NoError(t, err)
	require.Len(t, all, appenders, "every racing append is durable across both partitions")
	for i := 1; i < len(all); i++ {
		require.Greater(t, all[i].NID, all[i-1].NID, "ascending NID across partitions")
	}
}

func seedEventQueryFixture(t *testing.T, db libdb.DBManager, clock *movableClock) (day1, day2 time.Time) {
	t.Helper()
	ctx := context.Background()
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))
	day1 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	day2 = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	clock.Set(day1)
	for _, ev := range []*runtimetypes.Event{
		{WorkspaceID: testEventWS, Type: "q.report", Source: "svc-a", Subject: "m-1"},
		{WorkspaceID: testEventWS, Type: "q.status", Source: "svc-b", Subject: "m-1"},
		{WorkspaceID: "ws-other", Type: "q.report", Source: "svc-a", Subject: "m-9"},
	} {
		require.NoError(t, store.AppendEvent(ctx, ev))
	}
	clock.Set(day2)
	for _, ev := range []*runtimetypes.Event{
		{WorkspaceID: testEventWS, Type: "q.report", Source: "svc-b", Subject: "m-2"},
		{WorkspaceID: testEventWS, Type: "q.report", Source: "svc-a", Subject: "m-1"},
		{WorkspaceID: "ws-other", Type: "q.status", Source: "svc-b", Subject: "m-9"},
	} {
		require.NoError(t, store.AppendEvent(ctx, ev))
	}
	return day1, day2
}

func TestUnit_Events_GetEventsByTypeSpansPartitionsNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{}
	day1, day2 := seedEventQueryFixture(t, db, clock)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	got, err := store.GetEventsByType(ctx, testEventWS, "q.report", day1.Add(-time.Hour), day2.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, got, 3, "cross-partition range covers both days")
	require.True(t, got[0].Time.After(got[2].Time) || got[0].NID > got[2].NID, "newest first")
	for _, e := range got {
		require.Equal(t, testEventWS, e.WorkspaceID)
		require.Equal(t, "q.report", e.Type)
	}

	day1Only, err := store.GetEventsByType(ctx, testEventWS, "q.report", day1.Add(-time.Hour), day1.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, day1Only, 1, "the time range prunes to one partition")
}

func TestUnit_Events_GetEventsBySourceAndBySubject(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{}
	day1, day2 := seedEventQueryFixture(t, db, clock)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))
	from, to := day1.Add(-time.Hour), day2.Add(time.Hour)

	bySource, err := store.GetEventsBySource(ctx, testEventWS, "q.report", from, to, "svc-a", 100)
	require.NoError(t, err)
	require.Len(t, bySource, 2)
	for _, e := range bySource {
		require.Equal(t, "svc-a", e.Source)
	}

	bySubject, err := store.GetEventsBySubject(ctx, testEventWS, "q.report", from, to, "m-1", 100)
	require.NoError(t, err)
	require.Len(t, bySubject, 2, "one entity's history across partitions")
	for _, e := range bySubject {
		require.Equal(t, "m-1", e.Subject)
	}
}

func TestUnit_Events_GetEventTypesInRange(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{}
	day1, day2 := seedEventQueryFixture(t, db, clock)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	types, err := store.GetEventTypesInRange(ctx, testEventWS, day1.Add(-time.Hour), day2.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"q.report", "q.status"}, types)

	day2Only, err := store.GetEventTypesInRange(ctx, testEventWS, day2.Add(-time.Hour), day2.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"q.report"}, day2Only, "ws-other's day-2 status is invisible here")
}

func TestUnit_Events_ListRecentEventsPagesNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{}
	_, _ = seedEventQueryFixture(t, db, clock)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	page1, err := store.ListRecentEvents(ctx, testEventWS, 0, 3)
	require.NoError(t, err)
	require.Len(t, page1, 3)
	require.Greater(t, page1[0].NID, page1[1].NID, "descending nid")

	page2, err := store.ListRecentEvents(ctx, testEventWS, page1[2].NID, 3)
	require.NoError(t, err)
	require.Len(t, page2, 1, "the smallest nid of a page cursors the next")
	require.Less(t, page2[0].NID, page1[2].NID)
	for _, e := range append(page1, page2...) {
		require.Equal(t, testEventWS, e.WorkspaceID)
	}
}

func TestUnit_Events_DeleteEventsByTypeInRangeIsSurgical(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{}
	day1, day2 := seedEventQueryFixture(t, db, clock)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))

	require.NoError(t, store.DeleteEventsByTypeInRange(ctx, testEventWS, "q.report", day1.Add(-time.Hour), day1.Add(time.Hour)))

	remaining, err := store.GetEventsByType(ctx, testEventWS, "q.report", day1.Add(-time.Hour), day2.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, remaining, 2, "only the day-1 report is gone; day-2 reports survive")

	other, err := store.GetEventsByType(ctx, "ws-other", "q.report", day1.Add(-time.Hour), day2.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, other, 1, "another workspace's rows are untouched by the delete")

	status, err := store.GetEventsByType(ctx, testEventWS, "q.status", day1.Add(-time.Hour), day2.Add(time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, status, 1, "other types in the same range survive")
}

func TestUnit_Events_WorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	db := setupEventDB(t)
	clock := &movableClock{}
	day1, day2 := seedEventQueryFixture(t, db, clock)
	store := runtimetypes.NewEventStore(db, runtimetypes.WithEventClock(clock.Now))
	from, to := day1.Add(-time.Hour), day2.Add(time.Hour)

	since, err := store.ListEventsSince(ctx, "ws-other", 0, 100)
	require.NoError(t, err)
	require.Len(t, since, 2)
	for _, e := range since {
		require.Equal(t, "ws-other", e.WorkspaceID)
	}

	recent, err := store.ListRecentEvents(ctx, "ws-other", 0, 100)
	require.NoError(t, err)
	require.Len(t, recent, 2)

	byType, err := store.GetEventsByType(ctx, "ws-other", "q.report", from, to, 100)
	require.NoError(t, err)
	require.Len(t, byType, 1)
	require.Equal(t, "m-9", byType[0].Subject)

	types, err := store.GetEventTypesInRange(ctx, "ws-other", from, to, 100)
	require.NoError(t, err)
	require.Equal(t, []string{"q.report", "q.status"}, types)

	require.ErrorIs(t, errFromList(store.ListEventsSince(ctx, "", 0, 10)), runtimetypes.ErrEventMissingRequiredField)

	noWS := &runtimetypes.Event{Type: "q.report"}
	require.ErrorIs(t, store.AppendEvent(ctx, noWS), runtimetypes.ErrEventMissingRequiredField)
}
