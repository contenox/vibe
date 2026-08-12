package nativeturn

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/contenox/contenox/libacp"
)

// ErrClosed is returned by Start once the Registry has been Closed; a sentinel for
// errors.Is(err, ErrClosed).
var ErrClosed = errors.New("nativeturn: registry is closed")

// Registry owns every native session's in-flight turn, off any single connection,
// keyed by ACP session id; every method is safe for concurrent use.
type Registry struct {
	cfg Config

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu       sync.Mutex
	sessions map[libacp.SessionID]*turnSession
	closed   bool
}

// New returns a Registry rooted on a fresh serve context (call Close at shutdown); a
// non-positive JournalSize, TurnDeadline, or GraceWindow is floored to its default.
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

// Turn is a caller's handle to one session's turn: how a connected viewer awaits
// completion, detaches on connection drop, and (for session/cancel) cancels.
type Turn struct {
	ts *turnSession
}

// SessionID is the ACP session this turn serves.
func (t *Turn) SessionID() libacp.SessionID { return t.ts.sessionID }

// Done is closed when the turn's chain ends; Result is then readable.
func (t *Turn) Done() <-chan struct{} { return t.ts.done }

// Result returns the turn's outcome, only meaningful after Done is closed.
func (t *Turn) Result() Result {
	t.ts.mu.Lock()
	defer t.ts.mu.Unlock()
	return t.ts.result
}

// Await blocks until the turn completes (Result, true) or ctx is done first (zero
// Result, false, without disturbing the turn); a convenience over selecting on Done.
func (t *Turn) Await(ctx context.Context) (Result, bool) {
	select {
	case <-t.ts.done:
		return t.Result(), true
	case <-ctx.Done():
		return Result{}, false
	}
}

// Detach removes viewerID from this turn's fan-out, never cancelling the turn while
// another viewer remains; detaching the last one starts the grace window (Belt 1).
// Idempotent.
func (t *Turn) Detach(viewerID string) { t.ts.detach(viewerID) }

// Cancel ends this turn now and tears it down (the explicit session/cancel); any
// viewer awaiting completion unblocks with the cancelled result. Idempotent.
func (t *Turn) Cancel() { t.ts.teardown() }

// Start ensures a turn is running for sid and attaches viewer to it: an in-flight
// turn is joined (started false), otherwise a fresh turn is started on a
// serve-rooted, hard-deadline-bounded context (belt 2, started true); returns
// ErrClosed once the Registry is Closed.
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

	// Attach before launching the goroutine so the turn's first emit reaches it; the
	// journal is empty, so nothing replays.
	_ = ts.attach(context.Background(), viewer)
	go ts.run(fn)
	return &Turn{ts: ts}, true, nil
}

// AttachIfRunning attaches viewer to sid's in-flight turn if one exists, replaying
// the journal and joining the live fan-out; returns (nil, false, nil) when none
// exists (a finished turn is deliberately not attachable here).
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

// Cancel cancels sid's in-flight turn and tears it down (session/cancel), reporting
// whether one was present (no-op false if not); any viewer awaiting the turn unblocks
// with its cancelled result.
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

// Stop is the operator-surface twin of Cancel: it ends sid's turn and tears it down,
// reporting whether one was present. Idempotent.
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

// List returns a snapshot of every active turn, sorted by session id; a point-in-time
// report, not a transaction.
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

// ReapIdle sweeps every session and tears down the ones that must not linger (belt
// 4): finished with no viewers, unwatched past its grace deadline, or past its hard
// deadline plus one grace window; always returns nil.
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

// Close tears every in-flight turn down and cancels the Registry's root context,
// after which Start returns ErrClosed. Idempotent.
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

func (r *Registry) removeSession(sid libacp.SessionID, ts *turnSession) {
	r.mu.Lock()
	if cur, ok := r.sessions[sid]; ok && cur == ts {
		delete(r.sessions, sid)
	}
	r.mu.Unlock()
}
