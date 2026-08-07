package eventtrigger_test

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// loopType is the type the self-feeding chain both listens for and publishes.
// A fixture name rather than any shipped type: the loop is a property of the
// hop guard, not of a particular vocabulary.
const loopType = "test.events.loop"

// loopRunner is a chain that causes the very event that fired it — the
// trigger→chain→event cycle the hop budget exists to terminate. It publishes
// through the DualPublisher on the run context, so the appended event inherits
// the dispatcher's hop+1 exactly as a real chain's tool appends do.
type loopRunner struct {
	pub *eventlog.DualPublisher

	mu   sync.Mutex
	hops []int
}

func (r *loopRunner) RunChain(ctx context.Context, _ eventtrigger.Trigger, ev runtimetypes.Event) error {
	r.mu.Lock()
	r.hops = append(r.hops, ev.Hop)
	r.mu.Unlock()
	return r.pub.Publish(ctx, loopType, []byte(`{}`))
}

func (r *loopRunner) runs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.hops...)
}

// TestUnit_HopCeiling_TerminatesASelfFeedingTriggerCycle is the loop guard the
// design record flagged as existing-but-untested. A trigger fires a chain, the
// chain causes an event of the same type, and that event fires the trigger
// again. Nothing else stops this: the firings table dedups a (trigger, event)
// PAIR, and every generation is a new event with a new NID, so the claim never
// collides. The hop budget is the only ceiling.
//
// It asserts termination (the cascade ends at all), the arithmetic (exactly
// DefaultMaxHop+1 generations run, hops 0..MaxHop), and the record (the
// generation past the ceiling is claimed and marked refused, never silently
// dropped — a refusal that left no row would be indistinguishable from a
// trigger that simply never matched).
func TestUnit_HopCeiling_TerminatesASelfFeedingTriggerCycle(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	store := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS)

	holder := eventlog.NewTriggerHolder()
	pub := eventlog.NewDualPublisher(runtimetypes.NewEventStore(db), nil, "test", testWS, nil,
		eventlog.WithPublisherTrigger(holder))

	runner := &loopRunner{pub: pub}
	handler, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store:       store,
		WorkspaceID: testWS,
		Triggers:    []eventtrigger.Trigger{trigger("loop", loopType)},
		Runner:      runner,
	})
	require.NoError(t, err)
	holder.Set(handler)

	// The seed. Everything after it is the cycle feeding itself.
	require.NoError(t, pub.Publish(ctx, loopType, []byte(`{}`)))

	// Drain waits out the whole cascade: a generation's firing begins before
	// its parent's returns, so the gate never reaches zero mid-cycle. The wait
	// terminating at all IS the property under test.
	require.True(t, holder.Drain(30*eventlog.DefaultDrainTimeout), "the trigger cycle never terminated")

	// Hops 0..MaxHop run; hop MaxHop+1 is refused before its chain.
	require.Len(t, runner.runs(), eventtrigger.DefaultMaxHop+1,
		"the cycle must die out after exactly the hop budget's generations")
	for i, hop := range runner.runs() {
		require.Equal(t, i, hop, "generation %d must carry hop %d", i, i)
	}

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, firings, eventtrigger.DefaultMaxHop+2, "every generation is claimed, including the refused one")
	refused := 0
	for _, f := range firings {
		switch f.Status {
		case runtimetypes.EventFiringStatusRefused:
			refused++
			require.Contains(t, f.Error, "exceeds limit", "the refusal records WHY it refused")
		case runtimetypes.EventFiringStatusOK:
		default:
			t.Fatalf("unexpected firing status %q: %s", f.Status, f.Error)
		}
	}
	require.Equal(t, 1, refused, "exactly the generation past the ceiling is refused")

	events, err := runtimetypes.NewEventStore(db).ListEventsSince(ctx, testWS, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, eventtrigger.DefaultMaxHop+2,
		"the refused generation's event is still durable; only the FIRING stops")
	require.Equal(t, eventtrigger.DefaultMaxHop+1, events[len(events)-1].Hop)
}

// TestUnit_PrefixListener_MatchesSubtreeAndExactStaysExact proves the
// hierarchical listener at the dispatch seam: "ide.file.*" fires on every type
// under it, and an exact listener is unchanged by the feature.
func TestUnit_PrefixListener_MatchesSubtreeAndExactStaysExact(t *testing.T) {
	ctx := context.Background()
	db := setupDispatchDB(t)
	svc := eventlog.NewService(db, nil, nil)
	store := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), testWS)

	runner := &fakeRunner{}
	h, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store:       store,
		WorkspaceID: testWS,
		Triggers: []eventtrigger.Trigger{
			trigger("subtree", "ide.file.*"),
			trigger("exact", "ide.search.run"),
		},
		Runner: runner,
	})
	require.NoError(t, err)

	for _, evType := range []string{"ide.file.opened", "ide.file.edited", "ide.search.run", "ide.panel.toggled"} {
		ev := &runtimetypes.Event{WorkspaceID: testWS, Type: evType, Data: []byte(`{}`)}
		require.NoError(t, svc.Append(ctx, ev))
		h.HandleEvent(ctx, ev)
	}

	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: 100})
	require.NoError(t, err)
	fired := map[string]int{}
	for _, f := range firings {
		fired[f.TriggerName]++
	}
	require.Equal(t, 2, fired["subtree"], "both ide.file.* types fire the subtree listener")
	require.Equal(t, 1, fired["exact"], "the exact listener is unchanged")
	require.Len(t, firings, 3, "ide.panel.toggled matches neither listener")
}
