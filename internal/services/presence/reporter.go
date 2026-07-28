package presence

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/google/uuid"
)

// ReporterStore is the narrow slice of Store a Reporter's heartbeat needs.
// Kept an interface so a test can substitute a store that fails.
type ReporterStore interface {
	Register(ctx context.Context, rec Record) error
	Deregister(ctx context.Context, kind Kind, instanceID string) error
}

// Reporter owns one process's presence record and keeps it alive: it writes
// the record on start, renews it on a modest interval and whenever a caller
// signals a change, and best-effort deregisters on shutdown. Every store
// error is a shrug reported to the tracker; StartReporter never blocks or
// fails the process it observes.
type Reporter struct {
	store    ReporterStore
	tracker  libtracker.ActivityTracker
	interval time.Duration
	// initialDelay defers the first registration write past the boot-critical
	// embed/init window on the shared SQLite file (see run). Overridable for
	// tests via WithInitialDelay.
	initialDelay time.Duration

	mu  sync.Mutex
	rec Record

	kick     chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// ReporterOption customizes a Reporter.
type ReporterOption func(*Reporter)

// WithInterval overrides the heartbeat cadence (default DefaultHeartbeatInterval).
func WithInterval(d time.Duration) ReporterOption {
	return func(r *Reporter) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithTracker sets the ActivityTracker heartbeat failures are shrugged to.
// Defaults to libtracker.NoopTracker; the report never reaches
// StartReporter's caller — presence stays best-effort regardless.
func WithTracker(tracker libtracker.ActivityTracker) ReporterOption {
	return func(r *Reporter) {
		if tracker != nil {
			r.tracker = tracker
		}
	}
}

// StartReporter registers rec and starts renewing it in the background
// until ctx is cancelled (or Stop is called), then best-effort
// deregisters. It fills in blank identity fields (InstanceID, PID, Host,
// StartedAt); the caller supplies only Kind and what it knows. Never
// blocks — even the first write happens on the background goroutine.
func StartReporter(ctx context.Context, store ReporterStore, rec Record, opts ...ReporterOption) *Reporter {
	rctx, cancel := context.WithCancel(ctx)
	r := &Reporter{
		store:        store,
		tracker:      libtracker.NoopTracker{},
		interval:     DefaultHeartbeatInterval,
		initialDelay: DefaultInitialDelay,
		rec:          rec,
		kick:         make(chan struct{}, 1),
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	for _, o := range opts {
		if o != nil {
			o(r)
		}
	}
	if r.tracker == nil {
		r.tracker = libtracker.NoopTracker{}
	}
	if r.rec.InstanceID == "" {
		r.rec.InstanceID = uuid.NewString()
	}
	if r.rec.PID == 0 {
		r.rec.PID = os.Getpid()
	}
	if r.rec.Host == "" {
		r.rec.Host, _ = os.Hostname()
	}
	if r.rec.StartedAt.IsZero() {
		r.rec.StartedAt = time.Now().UTC()
	}
	go r.run(rctx)
	return r
}

// InstanceID is the id this reporter registered under (useful once StartReporter
// has minted one).
func (r *Reporter) InstanceID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec.InstanceID
}

// Update mutates the record under lock and prompts an immediate heartbeat,
// coalesced onto a depth-1 channel so a burst of events collapses to one
// extra write and Update never blocks its caller.
func (r *Reporter) Update(mutate func(rec *Record)) {
	if mutate == nil {
		return
	}
	r.mu.Lock()
	mutate(&r.rec)
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Stop cancels the reporter and waits for the background goroutine to finish its
// best-effort deregister. Safe to call more than once.
func (r *Reporter) Stop() {
	r.stopOnce.Do(r.cancel)
	<-r.done
}

func (r *Reporter) run(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// The initial registration is deferred past the boot-critical window
	// where schema/preset embedding writes the same SQLite file, so an eager
	// write cannot starve that init into "database is locked". A
	// session-event kick still writes immediately; ctx cancellation during
	// the delay deregisters cleanly.
	select {
	case <-ctx.Done():
		return
	case <-time.After(r.initialDelay):
	case <-r.kick:
	}
	r.write(ctx) // initial registration (best-effort)
	for {
		select {
		case <-ctx.Done():
			r.deregister()
			return
		case <-ticker.C:
			r.write(ctx)
		case <-r.kick:
			r.write(ctx)
		}
	}
}

func (r *Reporter) write(ctx context.Context) {
	r.mu.Lock()
	rec := r.rec
	r.mu.Unlock()
	rec.LastSeen = time.Now().UTC()
	if err := r.store.Register(ctx, rec); err != nil {
		reportErr, _, end := r.tracker.Start(ctx, "register", "presence_record", "kind", rec.Kind, "instance", rec.InstanceID)
		reportErr(fmt.Errorf("presence: heartbeat write failed (ignored): %w", err))
		end()
	}
}

// deregister runs on shutdown with a fresh bounded context (the reporter's own is
// already cancelled) so a clean exit can still remove the row. Best-effort: a
// failure just leaves the row to age out on its TTL.
func (r *Reporter) deregister() {
	r.mu.Lock()
	kind, id := r.rec.Kind, r.rec.InstanceID
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.store.Deregister(ctx, kind, id); err != nil {
		reportErr, _, end := r.tracker.Start(ctx, "deregister", "presence_record", "kind", kind, "instance", id)
		reportErr(fmt.Errorf("presence: deregister failed (ignored): %w", err))
		end()
	}
}

// WithInitialDelay overrides how long the reporter waits before its first
// registration write (default DefaultInitialDelay; see run for why it exists).
// Tests use 0 to register immediately.
func WithInitialDelay(d time.Duration) ReporterOption {
	return func(r *Reporter) { r.initialDelay = d }
}
