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
	// StateGrace: the chain is still executing but no viewer is attached, with a grace
	// timer counting down (belt 1); a reattach returns it to running, expiry cancels it.
	StateGrace = "grace"
	// StateFinished: the chain has ended and the session awaits viewer
	// detach / reaper cleanup.
	StateFinished = "finished"
	// StateSuspended: the chain suspended on a pending human approval and checkpointed
	// durably; lifecycle-wise a finished turn whose distinct state tells the operator
	// to wait for the approval, not "done".
	StateSuspended = "suspended"
)

// Result is the outcome of one turn, returned by a TurnFunc and read back by
// the connected viewer to resolve its libacp session/prompt RPC.
type Result struct {
	// StopReason is the ACP stop reason the connected client's prompt resolves with.
	StopReason libacp.StopReason
	// Err is a genuine execution failure the client must see as a JSON-RPC error; nil
	// for a clean end or a cancellation (which resolves as StopReasonCancelled with no
	// error).
	Err error
	// Suspended marks a turn whose chain parked on a pending human approval:
	// not a failure (Err is nil), and status reads StateSuspended instead of
	// StateFinished until reaped.
	Suspended bool
	// ApprovalID is the durable approval a Suspended turn is parked on, communicated to
	// the client since StopReason cannot express it; empty unless Suspended.
	ApprovalID string
	// DroppedContentKinds are the prompt content kinds the turn could not forward to
	// the model, ridden on Result so a reattaching client (which resolves its prompt
	// from here, not the original connection) still sees them; empty when the whole
	// prompt was forwarded.
	DroppedContentKinds []string
}

// TurnFunc runs one turn's work: ctx is the serve-rooted, hard-deadline-bounded turn
// context (belt 2), outliving the client that started it, and emit journals a
// session/update and fans it out to every attached viewer exactly once and in order.
type TurnFunc func(ctx context.Context, emit func(ctx context.Context, n libacp.SessionNotification)) Result

// TurnStatus is a point-in-time snapshot of one active turn (Registry.List/Get); a
// value copy, mutating it never affects the live turn.
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

type turnSession struct {
	reg       *Registry
	sessionID libacp.SessionID
	startedAt time.Time
	deadline  time.Time

	turnCtx context.Context
	cancel  context.CancelFunc

	done   chan struct{}
	result Result

	reaping atomic.Bool

	mu            sync.Mutex
	seq           uint64
	journal       *journal
	viewers       map[string]Viewer
	order         []string
	finished      bool
	graceTimer    *time.Timer
	graceDeadline time.Time
}

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

func (ts *turnSession) run(fn TurnFunc) {
	ts.markFinished(fn(ts.turnCtx, ts.emit))
}

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

func (ts *turnSession) isFinished() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.finished
}

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
