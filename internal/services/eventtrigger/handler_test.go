package eventtrigger_test

import (
	"context"

	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_InProcessHandler_FiresAndClaimsFiring proves the in-process
// handler fires a matching trigger synchronously and writes the same
// event_firings claim the dispatcher would.
func TestUnit_InProcessHandler_FiresAndClaimsFiring(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS)

	runner := &fakeRunner{}
	h, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store:       store,
		WorkspaceID: testWS,
		Triggers:    []eventtrigger.Trigger{trigger("t1", "test.events.report")},
		Runner:      runner,
	})
	require.NoError(t, err)

	ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{"i":1}`)}
	require.NoError(t, svc.Append(ctx, ev))
	h.HandleEvent(ctx, ev)

	runs := runner.snapshot()
	require.Len(t, runs, 1)
	require.Equal(t, ev.NID, runs[0].NID)
	require.Equal(t, 1, runs[0].Hop, "a fresh event's chain runs at hop+1 = 1")

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firings, 1)
	require.Equal(t, runtimetypes.EventFiringStatusOK, firings[0].Status)
	require.Equal(t, "t1", firings[0].TriggerName)
	require.Equal(t, ev.NID, firings[0].NID)
}

// TestUnit_InProcessHandler_DispatcherNeverDoubleFires proves the shared
// claim: an event the in-process handler already fired is skipped by the
// standalone dispatcher's catch-up drain.
func TestUnit_InProcessHandler_DispatcherNeverDoubleFires(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS)
	triggers := []eventtrigger.Trigger{trigger("t1", "test.events.report")}

	inprocRunner := &fakeRunner{}
	h, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store: store, WorkspaceID: testWS, Triggers: triggers, Runner: inprocRunner,
	})
	require.NoError(t, err)

	// One event fired live in-process, one appended with no handler running
	// (the catch-up case).
	live := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{"path":"live"}`)}
	require.NoError(t, svc.Append(ctx, live))
	h.HandleEvent(ctx, live)
	missed := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{"path":"missed"}`)}
	require.NoError(t, svc.Append(ctx, missed))

	dispatchRunner := &fakeRunner{}
	d, err := eventtrigger.New(eventtrigger.Deps{
		Log: svc, Store: store, WorkspaceID: testWS, Triggers: triggers,
		Runner: dispatchRunner, Poll: 5 * time.Millisecond,
	})
	require.NoError(t, err)
	stop := startDispatcher(t, d)
	// The drain is done once the cursor passes the last appended event.
	require.Eventually(t, func() bool {
		cursor, err := store.GetEventCursor(ctx, eventtrigger.DefaultConsumer)
		return err == nil && cursor >= missed.NID
	}, 10*time.Second, 5*time.Millisecond)
	stop()

	require.Len(t, inprocRunner.snapshot(), 1, "the live event fired in-process")
	runs := dispatchRunner.snapshot()
	require.Len(t, runs, 1, "the dispatcher fires only the event nothing handled live")
	require.Equal(t, missed.NID, runs[0].NID)

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firings, 2, "one claim per (trigger, event), whichever path fired it")
}

// TestUnit_InProcessHandler_StrandedFiringIsRetried is the lost-firing
// regression: a host that claimed a firing and died mid-chain (a `--wait` CLI
// tearing its engine and database down under the handler goroutine) leaves a
// 'running' row nothing ever finishes. The claim is INSERT OR IGNORE, so until
// the stale-claim takeover existed that row refused every later firing of the
// same (trigger, event) pair for good — including the catch-up dispatcher's,
// the one consumer documented to cover exactly this case.
func TestUnit_InProcessHandler_StrandedFiringIsRetried(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	triggers := []eventtrigger.Trigger{trigger("t1", "test.events.report")}
	died := time.Now().UTC()

	// The dead host: it claims, then the process ends before FinishEventFiring.
	strandedStore := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS,
		runtimetypes.WithEventFiringClock(func() time.Time { return died }))
	ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{}`)}
	require.NoError(t, svc.Append(ctx, ev))
	claimed, err := strandedStore.BeginEventFiring(ctx, "t1", ev.NID, "evt-dead-host")
	require.NoError(t, err)
	require.True(t, claimed)

	// A host arriving while the claim could still be live leaves it alone.
	tooSoon := &fakeRunner{}
	soonHandler, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store: runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS,
			runtimetypes.WithEventFiringClock(func() time.Time { return died.Add(time.Minute) })),
		WorkspaceID: testWS, Triggers: triggers, Runner: tooSoon,
	})
	require.NoError(t, err)
	soonHandler.HandleEvent(ctx, ev)
	require.Empty(t, tooSoon.snapshot(), "a claim inside the bound is a live host's, never stolen")

	// Past the bound the firing is retried and reaches a recorded outcome.
	retry := &fakeRunner{}
	retryHandler, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store: runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS,
			runtimetypes.WithEventFiringClock(func() time.Time { return died.Add(runtimetypes.StaleEventFiringClaim + time.Minute) })),
		WorkspaceID: testWS, Triggers: triggers, Runner: retry,
	})
	require.NoError(t, err)
	retryHandler.HandleEvent(ctx, ev)

	runs := retry.snapshot()
	require.Len(t, runs, 1, "the stranded firing is retried, not lost")
	require.Equal(t, ev.NID, runs[0].NID)

	firings, err := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS).ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firings, 1, "the retry reuses the claim row")
	require.Equal(t, runtimetypes.EventFiringStatusOK, firings[0].Status, "the row finally carries an outcome")
	require.NotEqual(t, "evt-dead-host", firings[0].RequestID, "and names the run that produced it")
}

// TestUnit_InProcessHandler_SkipsForeignWorkspaceAndSpentHops pins the two
// refusals: an event of another workspace never fires, and a hop past the
// budget is claimed refused without running.
func TestUnit_InProcessHandler_SkipsForeignWorkspaceAndSpentHops(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS)

	runner := &fakeRunner{}
	h, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store: store, WorkspaceID: testWS,
		Triggers: []eventtrigger.Trigger{trigger("t1", "test.events.report")},
		Runner:   runner,
	})
	require.NoError(t, err)

	foreign := &runtimetypes.Event{WorkspaceID: "ws-other", Type: "test.events.report", Data: []byte(`{}`)}
	require.NoError(t, svc.Append(ctx, foreign))
	h.HandleEvent(ctx, foreign)
	require.Empty(t, runner.snapshot(), "another workspace's event never fires here")

	spent := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{}`), Hop: eventtrigger.DefaultMaxHop + 1}
	require.NoError(t, svc.Append(ctx, spent))
	h.HandleEvent(ctx, spent)
	require.Empty(t, runner.snapshot(), "a spent hop budget refuses the firing")

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firings, 1, "the refusal is claimed; the foreign event is not")
	require.Equal(t, runtimetypes.EventFiringStatusRefused, firings[0].Status)
	require.Equal(t, spent.NID, firings[0].NID)
}

// TestUnit_InProcessHandler_RunErrorRecordedNeverPropagates pins the onError
// contract: a failing chain records status=error and HandleEvent returns
// normally.
func TestUnit_InProcessHandler_RunErrorRecordedNeverPropagates(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS)

	runner := &fakeRunner{failFor: map[string]bool{"t1": true}}
	h, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store: store, WorkspaceID: testWS,
		Triggers: []eventtrigger.Trigger{trigger("t1", "test.events.report")},
		Runner:   runner,
	})
	require.NoError(t, err)

	ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{}`)}
	require.NoError(t, svc.Append(ctx, ev))
	h.HandleEvent(ctx, ev)

	require.Len(t, runner.snapshot(), 1)
	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firings, 1)
	require.Equal(t, runtimetypes.EventFiringStatusError, firings[0].Status)
	require.Contains(t, firings[0].Error, "boom")
}

// TestUnit_EventlogAppend_FiresInProcessTrigger proves the service hook: a
// durable append hands the stored event (NID assigned) to the wired trigger
// asynchronously, and an append with no hook stays exactly as before.
func TestUnit_EventlogAppend_FiresInProcessTrigger(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)

	got := make(chan *runtimetypes.Event, 1)
	svc := eventlog.NewService(db, nil, nil, eventlog.WithTrigger(chanTrigger{ch: got}))

	ev := &runtimetypes.Event{WorkspaceID: testWS, Type: "test.events.report", Data: []byte(`{"i":9}`)}
	require.NoError(t, svc.Append(ctx, ev))
	select {
	case fired := <-got:
		require.Equal(t, ev.NID, fired.NID, "the hook sees the stored event, NID assigned")
	case <-time.After(5 * time.Second):
		t.Fatal("append never fired the in-process trigger hook")
	}
}

// TestUnit_DualPublisher_FiresInProcessTrigger proves the dual-write hook:
// a successful append fires the trigger with the stored envelope; the live
// publish contract is untouched (nil next stays valid).
func TestUnit_DualPublisher_FiresInProcessTrigger(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)

	got := make(chan *runtimetypes.Event, 1)
	pub := eventlog.NewDualPublisher(runtimetypes.NewEventStore(db), nil, "missionservice", testWS, nil,
		eventlog.WithSubjectField("missionId"),
		eventlog.WithPublisherTrigger(chanTrigger{ch: got}))

	require.NoError(t, pub.Publish(ctx, "missionservice.events.attention_asked", []byte(`{"missionId":"m-1","askId":"a-1"}`)))
	select {
	case fired := <-got:
		require.Equal(t, "missionservice.events.attention_asked", fired.Type)
		require.Equal(t, "m-1", fired.Subject)
		require.NotZero(t, fired.NID)
	case <-time.After(5 * time.Second):
		t.Fatal("dual-write append never fired the in-process trigger hook")
	}
}

// chanTrigger delivers each event to ch (the test's synchronization point).
type chanTrigger struct{ ch chan *runtimetypes.Event }

func (c chanTrigger) HandleEvent(_ context.Context, events ...*runtimetypes.Event) {
	for _, ev := range events {
		select {
		case c.ch <- ev:
		default:
		}
	}
}
