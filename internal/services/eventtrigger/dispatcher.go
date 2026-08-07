package eventtrigger

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	libbus "github.com/contenox/contenox/internal/libbus"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
)

// DefaultMaxHop is the dispatch-loop budget: an event whose hop exceeds it is
// refused, never fired. A trigger chain's own events carry hop+1, so a
// self-feeding trigger dies out after this many generations.
const DefaultMaxHop = 4

// DefaultConsumer is the cursor name the CLI dispatcher runs under.
const DefaultConsumer = "eventtrigger.dispatcher"

// ChainRunner executes one trigger's chain against one event. The context
// carries the firing's request ID (libtracker.ContextKeyRequestID) and the
// incremented hop (runtimetypes.WithEventHop) — implementations must thread it into
// the execution so appended events inherit both. A returned error is recorded
// on the firing row; it never stops the dispatch loop.
type ChainRunner interface {
	RunChain(ctx context.Context, t Trigger, ev runtimetypes.Event) error
}

// Deps are the dispatcher's collaborators. Log, Store, Runner, and
// WorkspaceID are required; the rest defaults (Tracker to Noop, Consumer to
// DefaultConsumer, MaxHop to DefaultMaxHop, Poll to 2s, Batch to 256).
type Deps struct {
	Log   eventlog.Service
	Store runtimetypes.EventFiringStore
	// WorkspaceID scopes the drain: the dispatcher processes ONLY events of
	// its own workspace (bob2's invariant, tenant → workspace — one
	// workspace's triggers never fire on another's events). Home-level
	// trigger files are defaults firing within this workspace, never
	// cross-workspace listeners.
	WorkspaceID string
	Triggers    []Trigger
	Runner      ChainRunner
	Tracker     libtracker.ActivityTracker
	// OnFiring observes every recorded firing outcome (the CLI's one line per
	// firing). err is nil on runtimetypes.EventFiringStatusOK.
	OnFiring func(t Trigger, ev runtimetypes.Event, status, requestID string, err error)
	Consumer string
	MaxHop   int
	// Poll is the backstop drain interval; live bus deliveries only shorten
	// the wait, they are never load-bearing.
	Poll  time.Duration
	Batch int
}

// Dispatcher is a durable named consumer over the event log: on Run it
// catches up from its cursor, then live-tails the bus, reconciling both paths
// through the firings table (an event may arrive via both; the
// (trigger, nid) claim dedups).
type Dispatcher struct {
	firingCore
	cursor int64
}

// firingCore is the shared claim/refuse/run/record engine behind both
// delivery paths: the Dispatcher's cursor drain and the in-process Handler.
// Both write the same event_firings claims, so a (trigger, nid) pair fires at
// most once no matter which path saw the event first.
type firingCore struct {
	deps Deps
	// triggers is kept as an ordered slice rather than a type-keyed map:
	// listen_for.type is a PATTERN (an exact type, or a dotted prefix ending
	// in ".*"), so the set a given event selects cannot be indexed by string
	// equality. Declaration order is firing order.
	triggers []Trigger
}

// newFiringCore validates the Deps both paths share (Log is the Dispatcher's
// own requirement, checked in New).
func newFiringCore(deps Deps) (firingCore, error) {
	if deps.Store == nil {
		return firingCore{}, fmt.Errorf("eventtrigger: Store is required")
	}
	if deps.Runner == nil {
		return firingCore{}, fmt.Errorf("eventtrigger: Runner is required")
	}
	if deps.WorkspaceID == "" {
		return firingCore{}, fmt.Errorf("eventtrigger: WorkspaceID is required")
	}
	if deps.Tracker == nil {
		deps.Tracker = libtracker.NoopTracker{}
	}
	if deps.Consumer == "" {
		deps.Consumer = DefaultConsumer
	}
	if deps.MaxHop <= 0 {
		deps.MaxHop = DefaultMaxHop
	}
	if deps.Poll <= 0 {
		deps.Poll = 2 * time.Second
	}
	if deps.Batch <= 0 {
		deps.Batch = 256
	}
	return firingCore{deps: deps, triggers: append([]Trigger(nil), deps.Triggers...)}, nil
}

// matching returns the triggers whose listener selects eventType, in
// declaration order.
func (d *firingCore) matching(eventType string) []Trigger {
	var matched []Trigger
	for _, t := range d.triggers {
		if eventlog.MatchesType(t.ListenFor.Type, eventType) {
			matched = append(matched, t)
		}
	}
	return matched
}

// liveSubjects returns the distinct EXACT event types the loaded triggers
// listen for. Bus subjects are exact strings, so a prefix pattern gets no live
// subscription: it is covered by the backstop poll, which is the only
// load-bearing path anyway (a live delivery never does more than shorten the
// wait).
func (d *firingCore) liveSubjects() []string {
	seen := map[string]bool{}
	subjects := []string{}
	for _, t := range d.triggers {
		if eventlog.IsPattern(t.ListenFor.Type) || seen[t.ListenFor.Type] {
			continue
		}
		seen[t.ListenFor.Type] = true
		subjects = append(subjects, t.ListenFor.Type)
	}
	return subjects
}

// New validates deps and builds a Dispatcher.
func New(deps Deps) (*Dispatcher, error) {
	if deps.Log == nil {
		return nil, fmt.Errorf("eventtrigger: Log is required")
	}
	core, err := newFiringCore(deps)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{firingCore: core}, nil
}

// nudgeBuffer bounds the live-delivery channel; a dropped nudge only delays
// a drain until the next poll tick.
const nudgeBuffer = 64

// Run loads the cursor, drains the backlog, then tails live deliveries and
// the poll tick until ctx is cancelled. Returns nil on cancellation.
func (d *Dispatcher) Run(ctx context.Context) error {
	cursor, err := d.deps.Store.GetEventCursor(ctx, d.deps.Consumer)
	if err != nil {
		return err
	}
	d.cursor = cursor

	// Live subscriptions attach before the catch-up drain so no event falls
	// between backlog and tail; overlap is deduped by the firings table.
	// Subscription failure is reported, not fatal — the poll tick still
	// drains the log.
	nudge := make(chan []byte, nudgeBuffer)
	var subs []libbus.Subscription
	for _, eventType := range d.liveSubjects() {
		sub, err := d.deps.Log.Subscribe(ctx, eventType, nudge)
		if err != nil {
			reportErr, _, end := d.deps.Tracker.Start(ctx, "subscribe", "event_dispatch", "type", eventType)
			reportErr(fmt.Errorf("eventtrigger: live subscription failed, falling back to polling: %w", err))
			end()
			continue
		}
		subs = append(subs, sub)
	}
	defer func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}()

	d.drain(ctx)

	ticker := time.NewTicker(d.deps.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-nudge:
			// A delivery is a wake-up only; the log is the source of truth, so
			// the payload (which varies by producer) is never interpreted.
			d.drain(ctx)
		case <-ticker.C:
			d.drain(ctx)
		}
	}
}

// drain processes every event past the cursor, advancing it per event so a
// crash resumes exactly where processing stopped.
func (d *Dispatcher) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := d.deps.Log.ListEventsSince(ctx, d.deps.WorkspaceID, d.cursor, d.deps.Batch)
		if err != nil {
			reportErr, _, end := d.deps.Tracker.Start(ctx, "drain", "event_dispatch", "after_nid", d.cursor)
			reportErr(err)
			end()
			return
		}
		if len(events) == 0 {
			return
		}
		for _, ev := range events {
			if ctx.Err() != nil {
				return
			}
			d.handle(ctx, ev)
			d.cursor = ev.NID
			// WithoutCancel: the event is fully handled — recording that fact
			// must survive a shutdown racing the last firing.
			if err := d.deps.Store.SetEventCursor(context.WithoutCancel(ctx), d.deps.Consumer, ev.NID); err != nil {
				reportErr, _, end := d.deps.Tracker.Start(ctx, "cursor", "event_dispatch", "nid", ev.NID)
				reportErr(err)
				end()
			}
		}
		if len(events) < d.deps.Batch {
			return
		}
	}
}

// handle fires every trigger matching ev's type. Each firing is claimed
// before it runs (dedup), refused when the hop budget is spent, and its
// outcome — including a chain failure — is recorded without ever stopping
// the loop.
func (d *firingCore) handle(ctx context.Context, ev runtimetypes.Event) {
	for _, t := range d.matching(ev.Type) {
		requestID := newRequestID()
		claimed, err := d.deps.Store.BeginEventFiring(ctx, t.Name, ev.NID, requestID)
		if err != nil {
			reportErr, _, end := d.deps.Tracker.Start(ctx, "claim", "event_firing", "trigger", t.Name, "nid", ev.NID)
			reportErr(err)
			end()
			continue
		}
		if !claimed {
			continue // already fired (the other arrival path won)
		}
		if ev.Hop > d.deps.MaxHop {
			refusal := fmt.Errorf("eventtrigger: event %d hop %d exceeds limit %d; refusing to fire %q", ev.NID, ev.Hop, d.deps.MaxHop, t.Name)
			d.finish(ctx, t, ev, runtimetypes.EventFiringStatusRefused, requestID, refusal)
			continue
		}
		runCtx := context.WithValue(ctx, libtracker.ContextKeyRequestID, requestID)
		runCtx = runtimetypes.WithEventHop(runCtx, ev.Hop+1)
		if err := d.deps.Runner.RunChain(runCtx, t, ev); err != nil {
			d.finish(ctx, t, ev, runtimetypes.EventFiringStatusError, requestID, err)
			continue
		}
		d.finish(ctx, t, ev, runtimetypes.EventFiringStatusOK, requestID, nil)
	}
}

// finish records a firing outcome and notifies the observer; a failed record
// write is reported, never propagated.
func (d *firingCore) finish(ctx context.Context, t Trigger, ev runtimetypes.Event, status, requestID string, firingErr error) {
	msg := ""
	if firingErr != nil {
		msg = firingErr.Error()
	}
	// WithoutCancel: the chain already ran; a shutdown racing this write must
	// not strand the firing as 'running'.
	if err := d.deps.Store.FinishEventFiring(context.WithoutCancel(ctx), t.Name, ev.NID, status, msg); err != nil {
		reportErr, _, end := d.deps.Tracker.Start(ctx, "record", "event_firing", "trigger", t.Name, "nid", ev.NID)
		reportErr(err)
		end()
	}
	if firingErr != nil {
		reportErr, _, end := d.deps.Tracker.Start(ctx, "fire", "event_trigger",
			"trigger", t.Name, "nid", ev.NID, "status", status, "request_id", requestID)
		reportErr(firingErr)
		end()
	}
	if d.deps.OnFiring != nil {
		d.deps.OnFiring(t, ev, status, requestID, firingErr)
	}
}

// newRequestID mints a firing's run correlation ID, the same register as
// libtracker.WithNewRequestID ("cli-…") with an "evt-" prefix so a firing's
// runs are recognizable in `contenox state list`.
func newRequestID() string {
	return fmt.Sprintf("evt-%016x", rand.Uint64())
}
