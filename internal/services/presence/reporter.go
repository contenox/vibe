package presence

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ReporterStore is the write side a Reporter drives — the narrow slice of Store a
// heartbeat needs. Kept an interface so a test can substitute a store that fails,
// which is exactly how the best-effort guard is proven.
type ReporterStore interface {
	Register(ctx context.Context, rec Record) error
	Deregister(ctx context.Context, kind Kind, instanceID string) error
}

// Reporter owns one process's presence record and keeps it alive: it writes the
// record on start, renews it on a modest interval AND whenever a caller signals a
// change (a session opened/closed), and best-effort deregisters on shutdown.
//
// It is best-effort to its core: StartReporter never blocks or fails the process
// it observes, and every store error is a logged shrug. An editor whose presence
// store is wedged still serves its user — it is merely absent from the board
// until its next successful heartbeat.
type Reporter struct {
	store    ReporterStore
	log      *slog.Logger
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

// WithLogger sets the logger heartbeat failures are shrugged to. Defaults to
// slog.Default at Debug — a presence hiccup is not the process's problem to
// shout about.
func WithLogger(l *slog.Logger) ReporterOption {
	return func(r *Reporter) {
		if l != nil {
			r.log = l
		}
	}
}

// StartReporter registers rec and starts renewing it in the background until ctx
// is cancelled (or Stop is called), then best-effort deregisters. It fills in the
// obvious identity fields the caller left blank (InstanceID, PID, Host,
// StartedAt) so a caller supplies only Kind and the facts it knows (Cwd,
// ClientName, ...).
//
// It NEVER blocks: even the first write happens on the background goroutine, so a
// slow or wedged store cannot delay the process's real startup. It always returns
// a usable *Reporter.
func StartReporter(ctx context.Context, store ReporterStore, rec Record, opts ...ReporterOption) *Reporter {
	rctx, cancel := context.WithCancel(ctx)
	r := &Reporter{
		store:        store,
		log:          slog.Default(),
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

// Update mutates the record under lock and prompts an immediate heartbeat, so a
// session-open/close event is reflected on the board without waiting for the next
// interval — the "renew on session events" trigger. The prompt is coalesced (a
// non-blocking send onto a depth-1 channel), so a burst of events collapses to
// one extra write and Update never blocks the caller (an ACP handler goroutine).
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

	// The initial registration is DEFERRED past the boot-critical window: at
	// process start the same SQLite file is being written by schema/preset
	// embedding, and an eager presence write here intermittently starved that
	// init into "database is locked" (observed on fresh `contenox serve` boots
	// the day presence landed). Presence is best-effort liveness — arriving on
	// the board a second late is free; failing another subsystem's boot is not.
	// A session-event kick still writes immediately (by then boot is past the
	// embed phase), and ctx cancellation during the delay deregisters cleanly.
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
		r.log.Debug("presence: heartbeat write failed (ignored)", "kind", rec.Kind, "instance", rec.InstanceID, "err", err)
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
		r.log.Debug("presence: deregister failed (ignored)", "kind", kind, "instance", id, "err", err)
	}
}

// WithInitialDelay overrides how long the reporter waits before its first
// registration write (default DefaultInitialDelay; see run for why it exists).
// Tests use 0 to register immediately.
func WithInitialDelay(d time.Duration) ReporterOption {
	return func(r *Reporter) { r.initialDelay = d }
}
