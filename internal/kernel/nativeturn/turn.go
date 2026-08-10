package nativeturn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/contenox/libacp"
)

// Turn states, the vocabulary of TurnStatus.State.
const (
	// StateRunning: the turn's chain is executing and at least one viewer is attached.
	StateRunning = "running"
	// StateGrace: the chain is still executing but no viewer is attached; a
	// grace timer is counting down (belt 1). A reattach returns it to
	// running; expiry cancels it.
	StateGrace = "grace"
	// StateFinished: the chain has ended and the session awaits viewer
	// detach / reaper cleanup.
	StateFinished = "finished"
	// StateSuspended: the chain suspended on a pending human approval and
	// checkpointed durably; its goroutine has ended. Lifecycle-wise it is a
	// finished turn (same teardown/reap rules), but the distinct state tells
	// the operator board to wait for the approval rather than "done". A
	// later resume attaches as a fresh turn with a fresh journal.
	StateSuspended = "suspended"
)

// Result is the outcome of one turn, returned by a TurnFunc and read back by
// the connected viewer to resolve its libacp session/prompt RPC.
type Result struct {
	// StopReason is the ACP stop reason the connected client's prompt resolves with.
	StopReason libacp.StopReason
	// Err is a genuine execution failure the client must see as a JSON-RPC
	// error. Nil for a clean end and for a cancellation, which resolves with
	// StopReasonCancelled and no error.
	Err error
	// Suspended marks a turn whose chain parked on a pending human approval:
	// not a failure (Err is nil), and status reads StateSuspended instead of
	// StateFinished until reaped.
	Suspended bool
	// ApprovalID is the durable approval a Suspended turn is parked on, and
	// is what the connected client is told it is waiting for — StopReason
	// alone cannot say it, since ACP has no suspended stop reason. Empty
	// unless Suspended.
	ApprovalID string
	// DroppedContentKinds are the prompt content kinds the turn could not
	// forward to the model. They ride the Result for the same reason
	// ApprovalID does: a reattaching client resolves its prompt from here,
	// not from the connection that started the turn, and a discarded
	// attachment must not become invisible by reconnecting. Empty when the
	// whole prompt was forwarded.
	DroppedContentKinds []string
}

// TurnFunc runs one turn's work. ctx is the serve-rooted, hard-deadline-bounded
// turn context (belt 2), not any connection's context, so the work outlives
// the client that started it. emit journals a session/update and fans it out
// to every attached viewer, exactly once and in order.
type TurnFunc func(ctx context.Context, emit func(ctx context.Context, n libacp.SessionNotification)) Result

// TurnStatus is a point-in-time snapshot of one active turn (Registry.List/Get).
// It is a value copy: mutating it never affects the live turn.
type TurnStatus struct {
	SessionID libacp.SessionID `json:"sessionId"`
	StartedAt time.Time        `json:"startedAt"`
	// Deadline is the hard wall-clock time this turn is terminated at (belt 2).
	Deadline time.Time `json:"deadline"`
	// Viewers is how many viewers are attached right now (0 in StateGrace).
	Viewers int `json:"viewers"`
	// State is one of StateRunning / StateGrace / StateFinished.
	State string `json:"state"`
}

// turnSession owns one session's in-flight (or just-finished) turn: the turn
// context + cancel, the replay journal, the viewer set, and the
// grace/teardown bookkeeping. Its own state is guarded by mu; the registry
// map that points at it is guarded separately by Registry.mu, always taken
// registry-first, so the fan-out (which takes only mu) never contends with
// a List or a removal.
type turnSession struct {
	reg       *Registry
	sessionID libacp.SessionID
	startedAt time.Time
	deadline  time.Time

	// turnCtx is the serve-rooted, WithDeadline lifeline every belt cancels
	// through: session/cancel, grace expiry, the hard deadline, and
	// Registry.Close all end the turn by cancelling it. cancel is idempotent.
	turnCtx context.Context
	cancel  context.CancelFunc

	// done is closed once the TurnFunc returns; result is readable after it closes.
	done   chan struct{}
	result Result

	// reaping claims the one-time teardown so the grace timer, the reaper,
	// an explicit cancel, and the natural finish path can all call
	// teardown() without double-acting.
	reaping atomic.Bool

	mu            sync.Mutex
	seq           uint64
	journal       *journal
	viewers       map[string]Viewer
	order         []string // attach order, for deterministic replay/fan-out
	finished      bool
	graceTimer    *time.Timer
	graceDeadline time.Time // zero unless a grace window is currently counting down
}

// emit assigns the next per-session monotonic Seq, journals the update, and
// fans it out to every attached viewer in attach order, all under mu, so a
// concurrent attach can never interleave with a live emit — a viewer sees
// each update either in its replayed backlog or live, never both or neither.
func (ts *turnSession) emit(ctx context.Context, n libacp.SessionNotification) {
	ts.mu.Lock()
	ts.seq++
	ev := Event{Seq: ts.seq, Update: n}
	ts.journal.append(ev)
	for _, id := range ts.order {
		_ = ts.viewers[id].Deliver(ctx, ev)
	}
	ts.mu.Unlock()
}

// attach registers v, replays the retained journal to it under mu, then
// joins the live fan-out. A reattach cancels any in-flight grace timer
// (belt 1). A duplicate viewer id is rejected.
func (ts *turnSession) attach(ctx context.Context, v Viewer) error {
	vid := v.ID()
	if vid == "" {
		return fmt.Errorf("nativeturn: viewer ID is required")
	}
	ts.mu.Lock()
	if _, dup := ts.viewers[vid]; dup {
		ts.mu.Unlock()
		return fmt.Errorf("nativeturn: viewer %q already attached to session %q", vid, ts.sessionID)
	}
	ts.viewers[vid] = v
	ts.order = append(ts.order, vid)
	// A reattach within the grace window keeps the turn alive.
	if ts.graceTimer != nil {
		ts.graceTimer.Stop()
		ts.graceTimer = nil
	}
	ts.graceDeadline = time.Time{}
	for _, ev := range ts.journal.snapshot() {
		_ = v.Deliver(ctx, ev)
	}
	ts.mu.Unlock()
	return nil
}

// detach removes viewer vid. If it was the last viewer and the turn is still
// in-flight, a grace timer starts (belt 1). Detaching the last viewer of an
// already-finished turn tears it down immediately. Detach never cancels a
// still-viewed turn.
func (ts *turnSession) detach(viewerID string) {
	ts.mu.Lock()
	if _, ok := ts.viewers[viewerID]; !ok {
		ts.mu.Unlock()
		return
	}
	delete(ts.viewers, viewerID)
	for i, id := range ts.order {
		if id == viewerID {
			ts.order = append(ts.order[:i], ts.order[i+1:]...)
			break
		}
	}
	empty := len(ts.viewers) == 0
	finished := ts.finished
	if empty && !finished {
		ts.graceDeadline = time.Now().Add(ts.reg.cfg.GraceWindow)
		if ts.graceTimer != nil {
			ts.graceTimer.Stop()
		}
		ts.graceTimer = time.AfterFunc(ts.reg.cfg.GraceWindow, ts.teardown)
	}
	ts.mu.Unlock()

	if empty && finished {
		ts.teardown()
	}
}

// run executes the turn body and records its outcome, on its own goroutine
// so the turn is fully off the connection.
func (ts *turnSession) run(fn TurnFunc) {
	ts.markFinished(fn(ts.turnCtx, ts.emit))
}

// markFinished records the turn's result, closes done, and releases the
// deadline timer. If nobody is watching, it tears the session down
// immediately; otherwise it lingers until its last viewer detaches.
// Idempotent.
func (ts *turnSession) markFinished(res Result) {
	ts.mu.Lock()
	if ts.finished {
		ts.mu.Unlock()
		return
	}
	ts.finished = true
	ts.result = res
	close(ts.done)
	viewers := len(ts.viewers)
	// A finished turn needs no grace window — its work is done and persisted.
	if ts.graceTimer != nil {
		ts.graceTimer.Stop()
		ts.graceTimer = nil
	}
	ts.graceDeadline = time.Time{}
	ts.mu.Unlock()

	// Release the WithDeadline timer promptly; cancelling an already-cancelled or
	// completed context is harmless.
	ts.cancel()
	if viewers == 0 {
		ts.teardown()
	}
}

// teardown is the one-time end-and-reclaim: cancels the turn context
// (ending the chain if still running), removes the session from the
// registry, and stops any grace timer. The reaping CAS makes it safe to
// call from every trigger exactly once.
func (ts *turnSession) teardown() {
	if !ts.reaping.CompareAndSwap(false, true) {
		return
	}
	ts.cancel()
	ts.reg.removeSession(ts.sessionID, ts)
	ts.mu.Lock()
	if ts.graceTimer != nil {
		ts.graceTimer.Stop()
		ts.graceTimer = nil
	}
	ts.mu.Unlock()
}

// isFinished reports whether the turn's chain has ended.
func (ts *turnSession) isFinished() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.finished
}

// status snapshots the turn for the operator surface.
func (ts *turnSession) status() TurnStatus {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	state := StateRunning
	switch {
	case ts.finished && ts.result.Suspended:
		state = StateSuspended
	case ts.finished:
		state = StateFinished
	case len(ts.viewers) == 0:
		state = StateGrace
	}
	return TurnStatus{
		SessionID: ts.sessionID,
		StartedAt: ts.startedAt,
		Deadline:  ts.deadline,
		Viewers:   len(ts.viewers),
		State:     state,
	}
}
