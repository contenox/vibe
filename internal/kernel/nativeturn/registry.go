package nativeturn

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/contenox/beam/libacp"
)

// ErrClosed is returned by Start once the Registry has been Closed. It is a
// sentinel so callers can branch on errors.Is(err, ErrClosed) if they ever need to.
var ErrClosed = errors.New("nativeturn: registry is closed")

// Registry owns every native session's in-flight turn, off any single
// connection. Created once at serve boot, shared across all per-connection
// Transports, and keyed by ACP session id: a turn survives a viewer detach,
// a reconnecting viewer replays the journal, and the anti-zombie belts
// guarantee no turn runs forever unwatched. Every method is safe for
// concurrent use.
type Registry struct {
	cfg Config

	// rootCtx is the long-lived context every turn descends from (via
	// WithDeadline), so a turn outlives the caller ctx that started it.
	// Close cancels it.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu       sync.Mutex
	sessions map[libacp.SessionID]*turnSession
	closed   bool
}

// New returns a Registry rooted on a fresh serve context. Call Close at
// shutdown to tear every in-flight turn down. A non-positive JournalSize,
// TurnDeadline, or GraceWindow is floored to its default.
func New(cfg Config) *Registry {
	if cfg.JournalSize <= 0 {
		cfg.JournalSize = DefaultJournalSize
	}
	if cfg.TurnDeadline <= 0 {
		cfg.TurnDeadline = DefaultTurnDeadline
	}
	if cfg.GraceWindow <= 0 {
		cfg.GraceWindow = DefaultGraceWindow
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Registry{
		cfg:        cfg,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
		sessions:   make(map[libacp.SessionID]*turnSession),
	}
}

// Turn is a caller's handle to one session's turn. It is how a connected viewer
// awaits completion, detaches on connection drop, and (for session/cancel) cancels.
type Turn struct {
	ts *turnSession
}

// SessionID is the ACP session this turn serves.
func (t *Turn) SessionID() libacp.SessionID { return t.ts.sessionID }

// Done is closed when the turn's chain ends; Result is then readable.
func (t *Turn) Done() <-chan struct{} { return t.ts.done }

// Result returns the turn's outcome. Only meaningful after Done is closed.
func (t *Turn) Result() Result {
	t.ts.mu.Lock()
	defer t.ts.mu.Unlock()
	return t.ts.result
}

// Await blocks until the turn completes (returning its Result and true) or ctx is
// done first (returning the zero Result and false, without disturbing the turn). It
// is a convenience over selecting on Done yourself.
func (t *Turn) Await(ctx context.Context) (Result, bool) {
	select {
	case <-t.ts.done:
		return t.Result(), true
	case <-ctx.Done():
		return Result{}, false
	}
}

// Detach removes viewerID from this turn's fan-out. It NEVER cancels the turn while
// another viewer remains; detaching the last viewer of an in-flight turn starts the
// grace window (Belt 1). Idempotent.
func (t *Turn) Detach(viewerID string) { t.ts.detach(viewerID) }

// Cancel ends this turn now and tears it down — the explicit user cancel
// (session/cancel). Any viewer awaiting completion unblocks with the turn's
// (cancelled) result. Idempotent.
func (t *Turn) Cancel() { t.ts.teardown() }

// Start ensures a turn is running for sid and attaches viewer to it. If a
// turn is already in-flight for sid, viewer joins it (journal replay + live
// fan-out) and started is false. Otherwise a fresh turn is started on a
// serve-rooted, hard-deadline-bounded context (belt 2), with viewer as its
// first attached viewer; started is true. Returns ErrClosed once the
// Registry is Closed.
func (r *Registry) Start(sid libacp.SessionID, fn TurnFunc, viewer Viewer) (*Turn, bool, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false, ErrClosed
	}
	if ts, ok := r.sessions[sid]; ok && !ts.isFinished() {
		r.mu.Unlock()
		if err := ts.attach(context.Background(), viewer); err != nil {
			return nil, false, err
		}
		return &Turn{ts: ts}, false, nil
	}
	ts := r.newTurnSession(sid)
	r.sessions[sid] = ts
	r.mu.Unlock()

	// Attach the starting viewer BEFORE launching the goroutine so the turn's first
	// emit already reaches it; the journal is empty, so this replays nothing.
	_ = ts.attach(context.Background(), viewer)
	go ts.run(fn)
	return &Turn{ts: ts}, true, nil
}

// AttachIfRunning attaches viewer to sid's in-flight turn if one exists,
// replaying the journal and joining the live fan-out. Returns (nil, false,
// nil) when no in-flight turn exists — a finished turn is deliberately not
// attachable here, since a reconnecting client's durable transcript already
// carries it.
func (r *Registry) AttachIfRunning(ctx context.Context, sid libacp.SessionID, viewer Viewer) (*Turn, bool, error) {
	r.mu.Lock()
	ts, ok := r.sessions[sid]
	r.mu.Unlock()
	if !ok || ts.isFinished() {
		return nil, false, nil
	}
	if err := ts.attach(ctx, viewer); err != nil {
		return nil, false, err
	}
	return &Turn{ts: ts}, true, nil
}

// Cancel cancels sid's in-flight turn and tears it down (session/cancel).
// Reports whether a turn was present; a cancel with no turn in flight is a
// no-op returning false. Any viewer awaiting the turn unblocks with its
// cancelled result.
func (r *Registry) Cancel(sid libacp.SessionID) bool {
	r.mu.Lock()
	ts, ok := r.sessions[sid]
	r.mu.Unlock()
	if !ok {
		return false
	}
	ts.teardown()
	return true
}

// Stop is the operator-surface twin of Cancel: it ends sid's turn and tears
// it down, reporting whether one was present. Distinct verb from the
// protocol-level session/cancel, though both reduce to the same teardown.
// Idempotent.
func (r *Registry) Stop(sid libacp.SessionID) bool { return r.Cancel(sid) }

// Get returns the status of sid's active turn, or ok=false when none is
// active (never started, or already reaped).
func (r *Registry) Get(sid libacp.SessionID) (TurnStatus, bool) {
	r.mu.Lock()
	ts, ok := r.sessions[sid]
	r.mu.Unlock()
	if !ok {
		return TurnStatus{}, false
	}
	return ts.status(), true
}

// List returns a snapshot of every active turn, sorted by session id. The
// live side is snapshotted under mu and each status read outside it, so a
// turn attaching or finishing concurrently lands on either side of the
// boundary — a point-in-time report, not a transaction.
func (r *Registry) List() []TurnStatus {
	r.mu.Lock()
	live := make([]*turnSession, 0, len(r.sessions))
	for _, ts := range r.sessions {
		live = append(live, ts)
	}
	r.mu.Unlock()

	out := make([]TurnStatus, 0, len(live))
	for _, ts := range live {
		out = append(out, ts.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// ReapIdle sweeps every session and tears down the ones that must not linger
// (belt 4, the periodic backstop behind the timer-driven grace path). A
// session is reaped when: it is finished with no viewers; it is in-flight,
// unwatched, and past its grace deadline; or wall-clock has passed its hard
// deadline plus one grace window. A running, watched turn inside its
// deadline is never reaped. Always returns nil.
func (r *Registry) ReapIdle(_ context.Context) error {
	r.mu.Lock()
	live := make([]*turnSession, 0, len(r.sessions))
	for _, ts := range r.sessions {
		live = append(live, ts)
	}
	r.mu.Unlock()

	now := time.Now()
	for _, ts := range live {
		ts.mu.Lock()
		finished := ts.finished
		viewers := len(ts.viewers)
		graceDeadline := ts.graceDeadline
		deadline := ts.deadline
		ts.mu.Unlock()

		reap := false
		switch {
		case finished && viewers == 0:
			reap = true
		case !finished && viewers == 0 && !graceDeadline.IsZero() && now.After(graceDeadline):
			reap = true
		case now.After(deadline.Add(r.cfg.GraceWindow)):
			reap = true
		}
		if reap {
			ts.teardown()
		}
	}
	return nil
}

// Close tears every in-flight turn down and cancels the Registry's root context. It
// is the runtime-shutdown hook; after Close, Start returns ErrClosed. Idempotent.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	live := make([]*turnSession, 0, len(r.sessions))
	for id, ts := range r.sessions {
		live = append(live, ts)
		delete(r.sessions, id)
	}
	r.mu.Unlock()

	for _, ts := range live {
		ts.teardown()
	}
	// Backstop: cancel the root so any turn whose teardown raced a fresh
	// registration still unwinds.
	r.rootCancel()
	return nil
}

// newTurnSession builds an in-flight turn bound to the registry root with a
// hard deadline (belt 2). It neither registers the session nor starts the
// goroutine — Start does both.
func (r *Registry) newTurnSession(sid libacp.SessionID) *turnSession {
	started := time.Now()
	turnCtx, cancel := context.WithDeadline(r.rootCtx, started.Add(r.cfg.TurnDeadline))
	return &turnSession{
		reg:       r,
		sessionID: sid,
		startedAt: started,
		deadline:  started.Add(r.cfg.TurnDeadline),
		turnCtx:   turnCtx,
		cancel:    cancel,
		done:      make(chan struct{}),
		journal:   newJournal(r.cfg.JournalSize),
		viewers:   make(map[string]Viewer),
	}
}

// removeSession deletes sid from the map iff it still points at ts, so a superseded
// or already-replaced turn's teardown never evicts its successor. It is the single
// place the map shrinks; teardown is its only caller.
func (r *Registry) removeSession(sid libacp.SessionID, ts *turnSession) {
	r.mu.Lock()
	if cur, ok := r.sessions[sid]; ok && cur == ts {
		delete(r.sessions, sid)
	}
	r.mu.Unlock()
}
