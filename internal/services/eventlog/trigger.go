package eventlog

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// DefaultDrainTimeout bounds TriggerHolder.Drain at a host's teardown. The
// drain is not the durability mechanism — a firing whose host dies mid-run is
// recovered by the stale-claim takeover in BeginEventFiring
// (runtimetypes.StaleEventFiringClaim). It closes only the common window, where
// a firing has already produced its outcome and needs nothing but the store
// write, so the budget is a database-write budget rather than a model one: it
// stays inside the couple of seconds the dispatcher's own backstop poll already
// treats as prompt, and a teardown never hangs waiting out a chain the reclaim
// would rescue anyway.
const DefaultDrainTimeout = 5 * time.Second

// Trigger receives every event after its durable append.
// Implementations isolate their own errors (recorded, never returned); a
// firing failure never aborts the append that produced the event.
type Trigger interface {
	HandleEvent(ctx context.Context, events ...*runtimetypes.Event)
}

// NoopTrigger is the default when no in-process dispatch is wired — bob2's
// NoopTrigger, so every append site has a valid Trigger to call.
type NoopTrigger struct{}

func (NoopTrigger) HandleEvent(context.Context, ...*runtimetypes.Event) {}

var _ Trigger = NoopTrigger{}

// TriggerHolder is a late-bound Trigger: hosts that construct publishers
// before their engine exists wire the holder at construction and Set the real
// handler once the engine is up. Unset, it is a NoopTrigger.
//
// It is also the drain seam: fireTrigger registers every firing it dispatches
// through a holder, so a host can Drain before tearing down the engine, bus,
// and database those firings still need. A host that mounts a handler directly
// instead of through a holder has no drain and relies solely on the stale-claim
// reclaim.
type TriggerHolder struct {
	t    atomic.Value // Trigger
	gate firingGate
}

// NewTriggerHolder returns an empty (noop) holder.
func NewTriggerHolder() *TriggerHolder { return &TriggerHolder{} }

// Set installs t as the delegate; a nil t is ignored.
func (h *TriggerHolder) Set(t Trigger) {
	if t != nil {
		h.t.Store(&t)
	}
}

func (h *TriggerHolder) HandleEvent(ctx context.Context, events ...*runtimetypes.Event) {
	if v, ok := h.t.Load().(*Trigger); ok {
		(*v).HandleEvent(ctx, events...)
	}
}

// Drain blocks until every firing already dispatched through this holder has
// returned, or timeout elapses; it reports whether the drain completed. A
// non-positive timeout waits indefinitely.
//
// Hosts call it before teardown. Without it, a firing that claimed its
// event_firings row and started a chain is killed when the process tears the
// engine, bus, and database down under it: nothing records an outcome, the row
// stays 'running', and every later claim of that (trigger, event) pair is
// refused until StaleEventFiringClaim elapses.
//
// Only firings outstanding when Drain is called are waited on — one dispatched
// afterwards is not, which is the teardown semantics: appends are over by then.
func (h *TriggerHolder) Drain(timeout time.Duration) bool {
	drained := h.gate.outstanding()
	if drained == nil {
		return true
	}
	if timeout <= 0 {
		<-drained
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drained:
		return true
	case <-timer.C:
		return false
	}
}

var _ Trigger = (*TriggerHolder)(nil)

// firingGate counts firings dispatched but not yet returned and hands a waiter
// a channel closed when that count reaches zero. Not a sync.WaitGroup: a fired
// chain appends events of its own, so Add races Wait during a teardown drain —
// documented WaitGroup misuse, and a runtime panic when the counter round-trips
// through zero while a waiter is parked.
type firingGate struct {
	mu sync.Mutex
	n  int
	// drained is nil exactly while n == 0; otherwise it is closed when the last
	// outstanding firing returns.
	drained chan struct{}
}

func (g *firingGate) begin() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == 0 {
		g.drained = make(chan struct{})
	}
	g.n++
}

func (g *firingGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	if g.n > 0 {
		return
	}
	g.n = 0
	if g.drained != nil {
		close(g.drained)
		g.drained = nil
	}
}

// outstanding returns the channel closed once the currently outstanding firings
// have all returned, or nil when there are none.
func (g *firingGate) outstanding() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drained
}

// fireTrigger dispatches event to trigger asynchronously — the in-process
// firing must never delay or abort the append (bob2's onError contract), and
// WithoutCancel keeps a firing alive past the appending request's own end. A
// holder's firing is registered BEFORE its goroutine starts: registering inside
// HandleEvent would leave the window between `go` and the delegate's first
// statement invisible to a concurrent Drain, and that window is exactly the one
// a teardown races.
func fireTrigger(ctx context.Context, trigger Trigger, event *runtimetypes.Event) {
	if trigger == nil {
		return
	}
	fireCtx := context.WithoutCancel(ctx)
	if h, ok := trigger.(*TriggerHolder); ok {
		h.gate.begin()
		go func() {
			defer h.gate.end()
			h.HandleEvent(fireCtx, event)
		}()
		return
	}
	go trigger.HandleEvent(fireCtx, event)
}
