package eventlog

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// DefaultDrainTimeout bounds TriggerHolder.Drain at a host's teardown. It is a
// database-write budget, not a model one: durability comes from the stale-claim
// takeover in BeginEventFiring, not from the drain.
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

// TriggerHolder is a late-bound Trigger: hosts that construct publishers before
// their engine exists wire the holder at construction and Set the real handler
// once the engine is up. Unset, it is a NoopTrigger. It is also the drain seam.
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
// non-positive timeout waits indefinitely. Only firings outstanding when Drain
// is called are waited on.
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

func (g *firingGate) outstanding() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drained
}

// The firing is registered before its goroutine starts, so a concurrent Drain cannot miss it.
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
