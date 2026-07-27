package nativeturn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/contenox/beam/libacp"
)

// Turn states, the vocabulary of TurnStatus.State.
const (
	// StateRunning: the turn's chain is executing and at least one viewer is
	// attached — the healthy, watched case.
	StateRunning = "running"
	// StateGrace: the turn's chain is still executing but NO viewer is attached; a
	// grace timer is counting down (Belt 1). A reattach returns it to running;
	// expiry cancels it.
	StateGrace = "grace"
	// StateFinished: the turn's chain has ended (its result is available) and the
	// session is awaiting viewer detach / reaper cleanup.
	StateFinished = "finished"
	// StateSuspended: the turn's chain SUSPENDED on a pending human approval
	// (S6) — checkpointed durably, its goroutine ended (that is the point: no
	// parked goroutine holds the run). Lifecycle-wise it is a finished turn
	// (same teardown/reap rules); the distinct state tells the operator board
	// "answer the approval to continue" apart from "done". A later resume in
	// this process attaches as a FRESH turn with a fresh journal — the
	// documented linkage is the session ID plus the run's request ID, under
	// which the durable engine-event journal is continuous across the
	// suspension (the in-memory journal here is per-turn and is not).
	StateSuspended = "suspended"
)

// Result is the outcome of one turn, returned by a TurnFunc and read back by the
// connected viewer to resolve its libacp session/prompt RPC.
type Result struct {
	// StopReason is the ACP stop reason the connected client's prompt resolves with.
	StopReason libacp.StopReason
	// Err is a genuine execution FAILURE the connected client must see surfaced as a
	// JSON-RPC error (e.g. a hard turn-deadline timeout, a setup error). It is nil
	// for a clean end and for a user/grace CANCELLATION — a cancellation resolves
	// with StopReasonCancelled and no error, per the ACP contract.
	Err error
	// Suspended marks a turn whose chain parked on a pending human approval and
	// checkpointed (S6): not a failure (Err is nil), and the turn's status reads
	// StateSuspended instead of StateFinished until it is reaped. The approval
	// card the permission flow already rendered stands in for further UI.
	Suspended bool
}

// TurnFunc runs one turn's work. ctx is the serve-rooted, hard-deadline-bounded
// turn context (Belt 2) — NOT any connection's context, which is the whole point:
// the work outlives the client that started it. emit journals a session/update and
// fans it out to every attached viewer (exactly-once, ordered). The returned Result
// is delivered to a viewer awaiting completion.
type TurnFunc func(ctx context.Context, emit func(ctx context.Context, n libacp.SessionNotification)) Result

// TurnStatus is a point-in-time snapshot of one active turn, for the operator
// surface (Registry.List/Get). It is a value copy: mutating it never affects the
// live turn. It mirrors agentinstance.InstanceStatus in intent — what is running,
// since when, how bounded, who is watching.
type TurnStatus struct {
	SessionID libacp.SessionID `json:"sessionId"`
	StartedAt time.Time        `json:"startedAt"`
	// Deadline is the hard wall-clock time this turn is terminated at (Belt 2).
	Deadline time.Time `json:"deadline"`
	// Viewers is how many viewers are attached right now (0 in StateGrace).
	Viewers int `json:"viewers"`
	// State is one of StateRunning / StateGrace / StateFinished.
	State string `json:"state"`
}

// turnSession owns one session's in-flight (or just-finished) turn: the turn
// context + cancel (the serve-rooted, deadline-bounded lifeline), the replay
// journal, the viewer set, and the grace/teardown bookkeeping. Its own mutable
// state is guarded by mu; the registry map that points at it is guarded separately
// by Registry.mu, and the two locks are always taken registry-first, never nested
// the other way, so the fan-out (which takes only mu) never contends with a List or
// a removal.
type turnSession struct {
	reg       *Registry
	sessionID libacp.SessionID
	startedAt time.Time
	deadline  time.Time

	// turnCtx is the serve-rooted, WithDeadline lifeline every belt cancels through:
	// session/cancel, grace expiry, the hard deadline, and Registry.Close all end the
	// turn by cancelling it. cancel is idempotent and also releases the deadline timer.
	turnCtx context.Context
	cancel  context.CancelFunc

	// done is closed once the TurnFunc returns; result is readable after it closes.
	done   chan struct{}
	result Result

	// reaping claims the one-time teardown so the grace timer, the reaper, an
	// explicit cancel, and the natural finish path can all call teardown() without
	// double-acting — the analogue of terminalservice's busy CAS guard in ReapIdle.
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

// emit assigns the next per-session monotonic Seq, journals the update, and fans it
// out to every attached viewer in attach order — all under mu, so a concurrent
// attach (which replays the journal under the same lock) can never interleave with
// a live emit. That mutual exclusion is what guarantees a viewer sees each update
// EITHER in its replayed backlog OR live, never both, never neither, never out of
// order.
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

// attach registers v, REPLAYS the retained journal to it under mu (so no live emit
// can slip between the replay and the join), then leaves it in the live fan-out. A
// reattach cancels any in-flight grace timer (Belt 1: someone is watching again). A
// duplicate viewer id is rejected, mirroring the external hub's "already attached".
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

// detach removes viewer vid. If it was the LAST viewer and the turn is still
// in-flight, a grace timer starts (Belt 1) — a reattach cancels it, expiry tears the
// turn down. Detaching the last viewer of an ALREADY-finished turn tears it down
// immediately (nothing left to watch, its output is persisted). Detach NEVER
// cancels a still-viewed turn: a viewer leaving is not a cancellation.
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

// run executes the turn body and records its outcome. It is the only caller of the
// TurnFunc, on its own goroutine, so the turn is fully off the connection.
func (ts *turnSession) run(fn TurnFunc) {
	ts.markFinished(fn(ts.turnCtx, ts.emit))
}

// markFinished records the turn's result, closes done (unblocking any awaiting
// viewer), and releases the deadline timer. If nobody is watching by the time the
// turn ends, it also tears the session down immediately; otherwise the session
// lingers until its last viewer detaches (which then tears it down). Idempotent.
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

// teardown is the one-time end-and-reclaim: it cancels the turn context (ending the
// chain if it is still running — the grace/deadline/explicit-cancel path), removes
// the session from the registry, and stops any grace timer. The reaping CAS makes it
// safe to call from every trigger (grace timer, reaper sweep, explicit Cancel/Stop,
// the natural finish path, Registry.Close) exactly once.
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
