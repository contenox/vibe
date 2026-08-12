package eventlog_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

const testWS = "ws-test"

func setupDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestSystem_EventService_AppendPublishesEnvelopeAfterDurableWrite(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{EventPoll: time.Millisecond, RequestPoll: time.Millisecond})
	t.Cleanup(func() { _ = bus.Close() })

	svc := eventlog.NewService(db, bus, nil)
	ch := make(chan []byte, 4)
	sub, err := svc.Subscribe(ctx, "test.events.live", ch)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.live", Source: "test", Data: json.RawMessage(`{"x":1}`)}
	require.NoError(t, svc.Append(ctx, ev))
	require.Greater(t, ev.NID, int64(0), "the row is durable before any publish")

	select {
	case payload := <-ch:
		var got runtimetypes.Event
		require.NoError(t, json.Unmarshal(payload, &got))
		require.Equal(t, ev.NID, got.NID, "the published envelope carries the assigned NID")
		require.Equal(t, "test.events.live", got.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("no live publish observed")
	}

	stored, err := svc.ListEventsSince(ctx, testWS, 0, 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, ev.NID, stored[0].NID)
}

// recordingPublisher captures forwarded publishes.
type recordingPublisher struct {
	subjects []string
	payloads [][]byte
}

func (r *recordingPublisher) Publish(_ context.Context, subject string, data []byte) error {
	r.subjects = append(r.subjects, subject)
	r.payloads = append(r.payloads, data)
	return nil
}

func TestUnit_EventLog_DualPublisher_AppendsThenForwardsVerbatim(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	store := runtimetypes.NewEventStore(db)
	next := &recordingPublisher{}
	pub := eventlog.NewDualPublisher(store, next, "missionservice", testWS, nil, eventlog.WithSubjectField("missionId"))

	payload := []byte(`{"missionId":"m-1","report":{"summary":"done"}}`)
	require.NoError(t, pub.Publish(runtimetypes.WithEventHop(ctx, 2), "missionservice.events.report_added", payload))

	require.Equal(t, []string{"missionservice.events.report_added"}, next.subjects, "live publish forwarded")
	require.Equal(t, payload, next.payloads[0], "forwarded bytes are verbatim — existing subscribers see no change")

	rows, err := store.ListEventsSince(ctx, testWS, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the same publish is durable in the log")
	require.Equal(t, "missionservice.events.report_added", rows[0].Type)
	require.Equal(t, "missionservice", rows[0].Source)
	require.Equal(t, "m-1", rows[0].Subject)
	require.Equal(t, 2, rows[0].Hop, "hop travels through the execution context into the appended row")
	require.JSONEq(t, string(payload), string(rows[0].Data))
}

func TestUnit_EventLog_DualPublisher_AppendFailureStillPublishes(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	store := runtimetypes.NewEventStore(db)
	next := &recordingPublisher{}
	pub := eventlog.NewDualPublisher(store, next, "missionservice", testWS, nil)

	// An empty subject cannot be appended (type required); the live publish
	// must still happen — the durable log never regresses the live path.
	require.NoError(t, pub.Publish(ctx, "", []byte(`{}`)))
	require.Len(t, next.payloads, 1)

	rows, err := store.ListEventsSince(ctx, testWS, 0, 10)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// blockingTrigger holds every firing until release is closed, and reports how
// many have returned — the shape a drain has to wait out.
type blockingTrigger struct {
	release chan struct{}
	done    atomic.Int64
}

func (b *blockingTrigger) HandleEvent(context.Context, ...*runtimetypes.Event) {
	<-b.release
	b.done.Add(1)
}

// TestUnit_TriggerHolder_DrainWaitsOutInFlightFirings is the teardown
// regression: a firing dispatched through the holder has claimed its
// event_firings row and is running a chain, and the host is about to close the
// engine, bus, and database under it. Drain is what makes the host wait for the
// outcome instead of killing the firing between its claim and its record.
func TestUnit_TriggerHolder_DrainWaitsOutInFlightFirings(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	blocker := &blockingTrigger{release: make(chan struct{})}
	holder := eventlog.NewTriggerHolder()
	holder.Set(blocker)
	pub := eventlog.NewDualPublisher(runtimetypes.NewEventStore(db), nil, "missionservice", testWS, nil,
		eventlog.WithPublisherTrigger(holder))

	for range 3 {
		require.NoError(t, pub.Publish(ctx, "missionservice.events.status_changed", []byte(`{"missionId":"m-1"}`)))
	}

	require.False(t, holder.Drain(50*time.Millisecond), "a drain must not claim success while firings are still running")
	require.Equal(t, int64(0), blocker.done.Load())

	close(blocker.release)
	require.True(t, holder.Drain(0), "an unbounded drain waits for every dispatched firing")
	require.Equal(t, int64(3), blocker.done.Load(), "every firing dispatched before the drain has returned")

	require.True(t, holder.Drain(0), "draining an idle holder is a no-op, not a block")
}

// TestUnit_TriggerHolder_DrainSurvivesFiringsAppendingTheirOwn is the reuse
// hazard a plain sync.WaitGroup would take: a fired chain appends events of its
// own, so new firings start while the teardown drain is parked.
func TestUnit_TriggerHolder_DrainSurvivesFiringsAppendingTheirOwn(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	holder := eventlog.NewTriggerHolder()
	pub := eventlog.NewDualPublisher(runtimetypes.NewEventStore(db), nil, "missionservice", testWS, nil,
		eventlog.WithPublisherTrigger(holder))

	var fired atomic.Int64
	stop := make(chan struct{})
	holder.Set(chainingTrigger{func() {
		if fired.Add(1) < 200 {
			select {
			case <-stop:
			default:
				_ = pub.Publish(ctx, "missionservice.events.status_changed", []byte(`{"missionId":"m-1"}`))
			}
		}
	}})

	require.NoError(t, pub.Publish(ctx, "missionservice.events.status_changed", []byte(`{"missionId":"m-1"}`)))
	// Drains repeatedly against a counter that keeps round-tripping through zero.
	for range 20 {
		holder.Drain(10 * time.Millisecond)
	}
	close(stop)
	require.True(t, holder.Drain(10*time.Second), "the drain settles once the cascade stops")
}

// chainingTrigger runs fn per event — a firing that appends events of its own.
type chainingTrigger struct{ fn func() }

func (c chainingTrigger) HandleEvent(_ context.Context, events ...*runtimetypes.Event) {
	for range events {
		c.fn()
	}
}

// TestUnit_DualPublisher_InheritsHopAcrossTheProcessBoundary pins the hop's
// survival of an exec: a chain that actuates by running `contenox …` starts a
// process with no execution context at all, so without the env var every event
// it appends would read hop 0 and a self-feeding trigger would never exhaust
// the dispatch budget.
func TestUnit_DualPublisher_InheritsHopAcrossTheProcessBoundary(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	store := runtimetypes.NewEventStore(db)
	// The literal, not just the constant: agentinstance.ChainHopEnvVar spells
	// the same name on the spawn side and cannot import this package.
	require.Equal(t, "CONTENOX_EVENT_HOP", eventlog.HopEnvVar)
	require.Equal(t, map[string]string{"CONTENOX_EVENT_HOP": "2"}, eventlog.SpawnEnv(runtimetypes.WithEventHop(ctx, 2)),
		"a spawn site outside this package gets the child's hop entry from here")
	require.Nil(t, eventlog.SpawnEnv(ctx), "an unfired spawn carries nothing")
	t.Setenv(eventlog.HopEnvVar, "3")

	// The round trip a child host closes: env back onto a context, so the
	// spawns IT makes forward the hop rather than restarting the budget.
	require.Equal(t, 3, runtimetypes.EventHopFromContext(eventlog.InheritHop(ctx)))
	require.Equal(t, 1, runtimetypes.EventHopFromContext(eventlog.InheritHop(runtimetypes.WithEventHop(ctx, 1))),
		"a context that already carries a hop is never overwritten by the environment")

	pub := eventlog.NewDualPublisher(store, nil, "missionservice", testWS, nil)
	require.NoError(t, pub.Publish(ctx, "missionservice.events.status_changed", []byte(`{"missionId":"m-1"}`)))
	// An in-process dispatch's own hop is the more specific answer and wins.
	require.NoError(t, pub.Publish(runtimetypes.WithEventHop(ctx, 1), "missionservice.events.status_changed", []byte(`{"missionId":"m-2"}`)))

	rows, err := store.ListEventsSince(ctx, testWS, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, 3, rows[0].Hop, "the spawner's hop is inherited verbatim, not restarted at 0")
	require.Equal(t, 1, rows[1].Hop, "an appending context that carries a hop overrides the inherited one")
}

// TestUnit_DualPublisher_MalformedInheritedHopIsNoHop pins the fail-open read:
// a broken variable must never stop a producer from publishing.
func TestUnit_DualPublisher_MalformedInheritedHopIsNoHop(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{"", "   ", "not-a-number", "-2", "0"} {
		db := setupDB(t)
		store := runtimetypes.NewEventStore(db)
		t.Setenv(eventlog.HopEnvVar, raw)
		pub := eventlog.NewDualPublisher(store, nil, "missionservice", testWS, nil)
		require.NoError(t, pub.Publish(ctx, "missionservice.events.status_changed", []byte(`{}`)))
		rows, err := store.ListEventsSince(ctx, testWS, 0, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, 0, rows[0].Hop, "%q is no inheritance, not an error", raw)
	}
}

// TestUnit_EventService_PruneThenAppendTargetsLivePartition is the stale-cache
// regression: the service memoizes the periods it ensured, so pruning must
// invalidate them — the next append re-creates the dropped period instead of
// writing into a partition that no longer exists.
func TestUnit_EventService_PruneThenAppendTargetsLivePartition(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	svc := eventlog.NewService(db, nil, nil)

	period := time.Now().UTC().Format("20060102")
	first := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.prune"}
	require.NoError(t, svc.Append(ctx, first))

	dropped, err := svc.PrunePartitionsBefore(ctx, time.Now().UTC().AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Equal(t, []string{period}, dropped, "the period the service just cached is dropped")

	second := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.prune"}
	require.NoError(t, svc.Append(ctx, second), "an append after the prune must find a live partition")
	require.Equal(t, first.NID+1, second.NID, "the NID sequence survives the prune")

	parts, err := runtimetypes.NewEventStore(db).ListEventPartitions(ctx)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, period, parts[0].Period, "the period was re-created, not assumed still there")

	rows, err := svc.ListEventsSince(ctx, testWS, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the pruned event is gone; the one appended after it is readable")
	require.Equal(t, second.NID, rows[0].NID)
}

// TestUnit_EventService_ValidatesQueryArguments pins the service-tier guard
// the store does not duplicate: an inverted range or an over-max limit is
// rejected before any partition is touched.
func TestUnit_EventService_ValidatesQueryArguments(t *testing.T) {
	ctx := context.Background()
	svc := eventlog.NewService(setupDB(t), nil, nil)
	now := time.Now().UTC()

	_, err := svc.GetEventsByType(ctx, testWS, "q.report", now, now.Add(-time.Hour), 10)
	require.ErrorIs(t, err, runtimetypes.ErrInvalidEventParameter)

	_, err = svc.GetEventsByType(ctx, testWS, "q.report", now.Add(-time.Hour), now, runtimetypes.MaxEventListLimit+1)
	require.ErrorIs(t, err, runtimetypes.ErrInvalidEventParameter)

	_, err = svc.GetEventsByType(ctx, "", "q.report", now.Add(-time.Hour), now, 10)
	require.ErrorIs(t, err, runtimetypes.ErrEventMissingRequiredField)

	require.ErrorIs(t, svc.Append(ctx, nil), runtimetypes.ErrInvalidEventParameter)
}
