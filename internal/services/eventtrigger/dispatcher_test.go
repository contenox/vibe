package eventtrigger_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libbus "github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

const testWS = "ws-dispatch"

func setupDispatchDB(t *testing.T) libdb.DBManager {
	t.Helper()
	db, err := libdb.NewSQLiteDBManager(context.Background(), filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// capturedRun is one RunChain invocation the fake runner observed.
type capturedRun struct {
	Trigger   string
	NID       int64
	Hop       int
	RequestID string
}

// fakeRunner records invocations; failFor names triggers whose runs error.
type fakeRunner struct {
	mu      sync.Mutex
	runs    []capturedRun
	failFor map[string]bool
}

func (f *fakeRunner) RunChain(ctx context.Context, tr eventtrigger.Trigger, ev runtimetypes.Event) error {
	reqID, _ := ctx.Value(libtracker.ContextKeyRequestID).(string)
	f.mu.Lock()
	f.runs = append(f.runs, capturedRun{
		Trigger:   tr.Name,
		NID:       ev.NID,
		Hop:       runtimetypes.EventHopFromContext(ctx),
		RequestID: reqID,
	})
	f.mu.Unlock()
	if f.failFor[tr.Name] {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeRunner) snapshot() []capturedRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRun{}, f.runs...)
}

func trigger(name, eventType string) eventtrigger.Trigger {
	return eventtrigger.Trigger{
		Name:      name,
		ListenFor: eventtrigger.Listener{Type: eventType},
		Type:      eventtrigger.TriggerTypeFireChain,
		Chain:     "chain-" + name + ".json",
	}
}

// startDispatcher runs d until the test ends or stop is called.
func startDispatcher(t *testing.T, d *eventtrigger.Dispatcher) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, d.Run(ctx))
	}()
	stop = func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

func waitRuns(t *testing.T, r *fakeRunner, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(r.snapshot()) >= want },
		10*time.Second, 5*time.Millisecond)
}

func TestSystem_Dispatcher_CatchUpAfterDowntimeFiresExactlyOnce(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil) // no bus: polling only, the downtime path
	store := mustFiringStore(t, db.WithoutTransaction(), testWS)

	// Events appended while no dispatcher ran.
	var nids []int64
	for i := range 3 {
		ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: fmt.Appendf(nil, `{"i":%d}`, i)}
		require.NoError(t, svc.Append(ctx, ev))
		nids = append(nids, ev.NID)
	}

	runner := &fakeRunner{}
	d, err := eventtrigger.New(eventtrigger.Deps{
		Log:         svc,
		Store:       store,
		WorkspaceID: testWS,
		Triggers:    []eventtrigger.Trigger{trigger("t1", "test.events.report")},
		Runner:      runner,
		Poll:        5 * time.Millisecond,
	})
	require.NoError(t, err)
	stop := startDispatcher(t, d)
	waitRuns(t, runner, 3)
	stop()

	runs := runner.snapshot()
	require.Len(t, runs, 3, "each backlog event fires exactly once")
	for i, run := range runs {
		require.Equal(t, nids[i], run.NID, "fired in append order")
		require.Equal(t, 1, run.Hop, "a fresh event's chain runs at hop+1 = 1")
		require.NotEmpty(t, run.RequestID)
	}

	cursor, err := store.GetEventCursor(ctx, eventtrigger.DefaultConsumer)
	require.NoError(t, err)
	require.Equal(t, nids[2], cursor, "the cursor is durable at the last processed NID")

	// A second downtime window, then a restarted dispatcher: only the new
	// event fires — the old ones are behind the cursor AND deduped.
	late := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report"}
	require.NoError(t, svc.Append(ctx, late))

	d2, err := eventtrigger.New(eventtrigger.Deps{
		Log:         svc,
		Store:       store,
		WorkspaceID: testWS,
		Triggers:    []eventtrigger.Trigger{trigger("t1", "test.events.report")},
		Runner:      runner,
		Poll:        5 * time.Millisecond,
	})
	require.NoError(t, err)
	stop2 := startDispatcher(t, d2)
	waitRuns(t, runner, 4)
	stop2()
	require.Len(t, runner.snapshot(), 4)
	require.Equal(t, late.NID, runner.snapshot()[3].NID)

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firings, 4)
	for _, f := range firings {
		require.Equal(t, runtimetypes.EventFiringStatusOK, f.Status)
	}
}

func TestSystem_Dispatcher_LivePlusCatchupDedupsThroughFirings(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	bus := libbus.NewSQLiteWithOptions(db.WithoutTransaction(), libbus.SQLiteBusOptions{EventPoll: time.Millisecond, RequestPoll: time.Millisecond})
	t.Cleanup(func() { _ = bus.Close() })
	svc := eventlog.NewService(db, bus, nil)
	store := mustFiringStore(t, db.WithoutTransaction(), testWS)

	runner := &fakeRunner{}
	d, err := eventtrigger.New(eventtrigger.Deps{
		Log:         svc,
		Store:       store,
		WorkspaceID: testWS,
		Triggers:    []eventtrigger.Trigger{trigger("t1", "test.events.live")},
		Runner:      runner,
		Poll:        5 * time.Millisecond,
	})
	require.NoError(t, err)
	startDispatcher(t, d)

	// Live append (published on the bus) rides both the nudge and the poll.
	ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.live"}
	require.NoError(t, svc.Append(ctx, ev))
	waitRuns(t, runner, 1)

	// Redundant nudges — a duplicate live delivery — must not re-fire the
	// already-claimed (trigger, nid).
	require.NoError(t, bus.Publish(ctx, "test.events.live", []byte(`nudge`)))
	require.NoError(t, bus.Publish(ctx, "test.events.live", []byte(`nudge`)))
	time.Sleep(50 * time.Millisecond)
	require.Len(t, runner.snapshot(), 1, "live + catch-up overlap fires once")

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firings, 1)
	require.Equal(t, ev.NID, firings[0].NID)
}

func TestUnit_Dispatcher_RefusesEventsPastHopLimit(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := mustFiringStore(t, db.WithoutTransaction(), testWS)

	over := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.loop", Hop: 5}
	require.NoError(t, svc.Append(ctx, over))
	atLimit := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.loop", Hop: 4}
	require.NoError(t, svc.Append(ctx, atLimit))

	runner := &fakeRunner{}
	d, err := eventtrigger.New(eventtrigger.Deps{
		Log:         svc,
		Store:       store,
		WorkspaceID: testWS,
		Triggers:    []eventtrigger.Trigger{trigger("t1", "test.events.loop")},
		Runner:      runner,
		Poll:        5 * time.Millisecond,
	})
	require.NoError(t, err)
	stop := startDispatcher(t, d)
	waitRuns(t, runner, 1)
	stop()

	runs := runner.snapshot()
	require.Len(t, runs, 1, "hop > 4 is refused, hop == 4 still fires")
	require.Equal(t, atLimit.NID, runs[0].NID)
	require.Equal(t, 5, runs[0].Hop, "the fired chain runs at the event's hop+1")

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firings, 2)
	byNID := map[int64]runtimetypes.EventFiring{}
	for _, f := range firings {
		byNID[f.NID] = f
	}
	require.Equal(t, runtimetypes.EventFiringStatusRefused, byNID[over.NID].Status)
	require.Contains(t, byNID[over.NID].Error, "hop 5 exceeds limit 4")
	require.Equal(t, runtimetypes.EventFiringStatusOK, byNID[atLimit.NID].Status)
}

func TestUnit_Dispatcher_ChainFailureNeverStallsOtherTriggers(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := mustFiringStore(t, db.WithoutTransaction(), testWS)

	first := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.mixed"}
	require.NoError(t, svc.Append(ctx, first))
	second := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.mixed"}
	require.NoError(t, svc.Append(ctx, second))

	runner := &fakeRunner{failFor: map[string]bool{"broken": true}}
	d, err := eventtrigger.New(eventtrigger.Deps{
		Log:         svc,
		Store:       store,
		WorkspaceID: testWS,
		Triggers: []eventtrigger.Trigger{
			trigger("broken", "test.events.mixed"),
			trigger("healthy", "test.events.mixed"),
		},
		Runner: runner,
		Poll:   5 * time.Millisecond,
	})
	require.NoError(t, err)
	stop := startDispatcher(t, d)
	waitRuns(t, runner, 4)
	stop()

	require.Len(t, runner.snapshot(), 4, "both triggers fire on both events despite failures")

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firings, 4)
	for _, f := range firings {
		if f.TriggerName == "broken" {
			require.Equal(t, runtimetypes.EventFiringStatusError, f.Status)
			require.Contains(t, f.Error, "boom")
		} else {
			require.Equal(t, runtimetypes.EventFiringStatusOK, f.Status)
		}
	}

	cursor, err := store.GetEventCursor(ctx, eventtrigger.DefaultConsumer)
	require.NoError(t, err)
	require.Equal(t, second.NID, cursor, "failures never wedge the cursor")
}

// TestSystem_Dispatcher_TwoWorkspacesNeverCrossFire ports bob2's tenant
// isolation to workspaces: two dispatchers over one database, one per
// workspace, same trigger name and event type — each fires exactly its own
// workspace's event, and neither cursor sees the other's.
func TestSystem_Dispatcher_TwoWorkspacesNeverCrossFire(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)

	evA := &runtimetypes.Event{WorkspaceID: "ws-a", Type: "test.events.iso"}
	require.NoError(t, svc.Append(ctx, evA))
	evB := &runtimetypes.Event{WorkspaceID: "ws-b", Type: "test.events.iso"}
	require.NoError(t, svc.Append(ctx, evB))

	runnerA := &fakeRunner{}
	runnerB := &fakeRunner{}
	storeA := mustFiringStore(t, db.WithoutTransaction(), "ws-a")
	storeB := mustFiringStore(t, db.WithoutTransaction(), "ws-b")
	newDispatcher := func(ws string, store runtimetypes.EventFiringStore, runner *fakeRunner) *eventtrigger.Dispatcher {
		d, err := eventtrigger.New(eventtrigger.Deps{
			Log:         svc,
			Store:       store,
			WorkspaceID: ws,
			Triggers:    []eventtrigger.Trigger{trigger("iso", "test.events.iso")},
			Runner:      runner,
			Poll:        5 * time.Millisecond,
		})
		require.NoError(t, err)
		return d
	}
	stopA := startDispatcher(t, newDispatcher("ws-a", storeA, runnerA))
	stopB := startDispatcher(t, newDispatcher("ws-b", storeB, runnerB))
	waitRuns(t, runnerA, 1)
	waitRuns(t, runnerB, 1)
	time.Sleep(50 * time.Millisecond)
	stopA()
	stopB()

	runsA, runsB := runnerA.snapshot(), runnerB.snapshot()
	require.Len(t, runsA, 1, "workspace A fires exactly its own event")
	require.Equal(t, evA.NID, runsA[0].NID)
	require.Len(t, runsB, 1, "workspace B fires exactly its own event")
	require.Equal(t, evB.NID, runsB[0].NID)

	firingsA, err := storeA.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firingsA, 1)
	require.Equal(t, "ws-a", firingsA[0].WorkspaceID)
	firingsB, err := storeB.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firingsB, 1)
	require.Equal(t, "ws-b", firingsB[0].WorkspaceID)

	cursorA, err := storeA.GetEventCursor(ctx, eventtrigger.DefaultConsumer)
	require.NoError(t, err)
	require.Equal(t, evA.NID, cursorA, "A's cursor tracks only A's stream")
	cursorB, err := storeB.GetEventCursor(ctx, eventtrigger.DefaultConsumer)
	require.NoError(t, err)
	require.Equal(t, evB.NID, cursorB, "B's cursor tracks only B's stream")
}
