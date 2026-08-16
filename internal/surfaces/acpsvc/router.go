package acpsvc

import (
	"context"
	"errors"
	"sync"

	"github.com/contenox/contenox/libacp"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// ErrNoBoundSession reports that no live ACP transport holds the contenox
// session named in the request context. AskApproval's caller falls back to
// the approval-API path instead of hanging on an unanswerable prompt.
var ErrNoBoundSession = errors.New("acpsvc: no ACP transport bound to contenox session")

// SessionRouter maps a contenox session id to every ACP Transport that holds
// it. One engine may be driven by several connections at once — serve's
// WebSockets, and the ACP profile's stdio connection alongside every relay
// attachment — and a session is one thing that several surfaces watch, not a
// thing one surface owns.
//
// Safe for concurrent use, and every method is nil-safe so a caller serving
// exactly one connection may leave Deps.SessionRouter unset.
//
// # A session is shared, not owned
//
// A transport joins a session when it creates, loads, resumes or adopts it, and
// again on every turn it prompts (Transport.claimSessionRouting). Joining does
// not evict anyone: a phone and a desk attached to one runtime both hold the
// session, and both are addressed.
//
// What that buys is the property the surfaces are expected to have — the same
// transcript, the same tool cards, the same approvals, wherever you are
// looking. The durable session already gave a late attacher the scrollback
// through session/load's replay; holding every transport is what extends that
// to the live tail.
//
// Order is most-recent-join first, which matters only to [transportFor], the
// one address left that must resolve to exactly one connection.
//
// # Asking several clients one question
//
// [AskApproval] asks every transport holding the session and takes the first
// real answer, cancelling the rest — libacp turns that cancellation into
// $/cancelRequest, so the losing cards are torn down rather than left stale on
// a second screen.
//
// This deliberately replaces a single-owner rule whose stated reason was that
// fanning out "would put one question in front of two humans and take whichever
// answered first as the verdict". The common case is not two humans; it is one
// human with two screens, and for them first-answer-wins is the behaviour they
// expect. A deployment that genuinely needs one destination states it in
// [SessionRouter.askTargets] rather than by narrowing the routing table, which
// is the seam an approval-forwarding surface plugs into.
//
// Nobody attached is still its own case: ErrNoBoundSession rather than an
// arbitrary connection, because what an unanswerable question means is the
// caller's to decide.
//
// # Leaving
//
// A connection's close releases its entries (Transport.releaseSessionRouting)
// and leaves every other holder standing. An outstanding request on the closed
// connection fails as ErrConnectionClosed on the connection that was actually
// asked; ErrNoBoundSession is reported only once the last holder is gone.
type SessionRouter struct {
	mu       sync.RWMutex
	bindings map[string][]*Transport
}

// NewSessionRouter returns an empty router ready to be shared across a
// serve process's ACP WebSocket transports.
func NewSessionRouter() *SessionRouter {
	return &SessionRouter{bindings: make(map[string][]*Transport)}
}

// bind records that t holds contenoxSessionID, most-recent join first. A
// transport already holding it moves to the front rather than being added
// twice, so a re-claim on every turn cannot grow the list.
func (r *SessionRouter) bind(contenoxSessionID string, t *Transport) {
	if r == nil || contenoxSessionID == "" || t == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.bindings[contenoxSessionID]
	for i, existing := range held {
		if existing == t {
			held = append(held[:i], held[i+1:]...)
			break
		}
	}
	r.bindings[contenoxSessionID] = append([]*Transport{t}, held...)
}

// unbind drops t from contenoxSessionID's holders and leaves the others alone.
// The key is deleted once the last holder goes, so transportsFor reports an
// empty session as unheld rather than as a session with no holders.
func (r *SessionRouter) unbind(contenoxSessionID string, t *Transport) {
	if r == nil || contenoxSessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.bindings[contenoxSessionID]
	for i, existing := range held {
		if existing == t {
			held = append(held[:i], held[i+1:]...)
			break
		}
	}
	if len(held) == 0 {
		delete(r.bindings, contenoxSessionID)
		return
	}
	r.bindings[contenoxSessionID] = held
}

// transportsFor returns every transport holding contenoxSessionID, as a copy:
// callers write to connections while holding no lock, and a stalled write must
// never be reached through the router's own mutex.
func (r *SessionRouter) transportsFor(contenoxSessionID string) []*Transport {
	if r == nil || contenoxSessionID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	held := r.bindings[contenoxSessionID]
	if len(held) == 0 {
		return nil
	}
	out := make([]*Transport, len(held))
	copy(out, held)
	return out
}

// transportFor returns the most recent holder. It exists for the addresses that
// must resolve to exactly one connection — delivering a prompt to two of them
// would run the turn twice — and not for anything a human reads.
func (r *SessionRouter) transportFor(contenoxSessionID string) (*Transport, bool) {
	held := r.transportsFor(contenoxSessionID)
	if len(held) == 0 {
		return nil, false
	}
	return held[0], true
}

// TransportResolver picks the connection a proxied tool call should act
// through for the session in ctx, or nil when no client is attached to it.
//
// Nil is an ordinary answer, not a failure: a host running a chain that no
// client is watching has no editor to read a file through, and the tools fall
// back to the operating system exactly as they do for a client that advertises
// no filesystem capability.
type TransportResolver func(context.Context) *Transport

// TransportForContext resolves the connection a tool call should act through:
// the most recent transport holding the contenox session in ctx, or nil when
// nothing holds it.
//
// Tools that proxy work to the client — reading a file through the editor so
// unsaved buffers are seen, running a command in the client's terminal — need
// *a* connection, not every connection, which is why this resolves to one
// holder rather than fanning out the way [AskApproval] does.
//
// It takes ctx rather than being a plain accessor because which connection is
// correct depends on which session is running: a host serving several relay
// attachments has no single "current" transport, and a process-wide one would
// send a phone session's file reads through whatever connection happened to
// start first.
func (r *SessionRouter) TransportForContext(ctx context.Context) *Transport {
	if r == nil || ctx == nil {
		return nil
	}
	contenoxSessionID, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	t, ok := r.transportFor(contenoxSessionID)
	if !ok {
		return nil
	}
	return t
}

// askTargets selects which of a session's holders are asked to approve. It is
// the one place a destination policy belongs: today every holder is asked, and
// a deployment that forwards approvals to one surface narrows the list here
// rather than by making the session single-holder again — the difference is
// that narrowing here still leaves every other surface watching the transcript.
func (r *SessionRouter) askTargets(held []*Transport) []*Transport { return held }

// AskApproval drives session/request_permission on every transport holding the
// contenox session in ctx (runtimetypes.SessionIDContextKey) and resolves with
// the first real answer.
//
// A deny is an answer and so is an approve; both resolve immediately and cancel
// the outstanding requests on the other holders, which reaches them as
// $/cancelRequest and clears the card. An error is not an answer: a connection
// that drops mid-question leaves the remaining screens asked, which is the
// whole point of asking them.
//
// Returns ErrNoBoundSession only when nothing holds the session. When every
// holder failed, the first error is returned rather than ErrNoBoundSession —
// the question was asked and went unanswered, which is not the same as having
// nowhere to ask.
func (r *SessionRouter) AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	contenoxSessionID, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	targets := r.askTargets(r.transportsFor(contenoxSessionID))
	if len(targets) == 0 {
		return false, ErrNoBoundSession
	}
	if len(targets) == 1 {
		return targets[0].AskApproval(ctx, req)
	}

	askCtx, cancelLosers := context.WithCancel(ctx)
	defer cancelLosers()

	type verdict struct {
		approved bool
		err      error
	}
	answers := make(chan verdict, len(targets))
	for _, target := range targets {
		go func(t *Transport) {
			approved, err := t.AskApproval(askCtx, req)
			answers <- verdict{approved: approved, err: err}
		}(target)
	}

	var firstErr error
	for range targets {
		got := <-answers
		if got.err == nil {
			return got.approved, nil
		}
		if firstErr == nil {
			firstErr = got.err
		}
	}
	return false, firstErr
}

// DeliverToContenoxSession routes an out-of-band mission report to every live
// connection holding contenoxSessionID, returning ErrSessionNotLive when none
// does. A report is something to read, so every screen gets it; delivery to one
// holder succeeding is enough to call it delivered.
func (r *SessionRouter) DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error {
	held := r.transportsFor(contenoxSessionID)
	if len(held) == 0 {
		return ErrSessionNotLive
	}
	var firstErr error
	delivered := false
	for _, t := range held {
		if err := t.DeliverToContenoxSession(ctx, contenoxSessionID, n); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		delivered = true
	}
	if delivered {
		return nil
	}
	return firstErr
}

// mirror copies notif to every holder of contenoxSessionID except origin, which
// has already written it to its own connection.
//
// It never blocks: each copy goes onto the target's own bounded queue (see
// mirror.go), because ndjsonWriter.Write takes a mutex and writes straight to
// the socket — a second screen that stopped reading would otherwise stall the
// turn that is feeding it.
//
// Delivery here is best-effort by design. The session's transcript is durable
// and session/load replays it, so a surface that misses live frames re-syncs by
// reattaching; that is the same property that lets a phone join a session that
// started at a desk.
func (r *SessionRouter) mirror(origin *Transport, contenoxSessionID string, notif libacp.SessionNotification) {
	if r == nil || contenoxSessionID == "" {
		return
	}
	for _, t := range r.transportsFor(contenoxSessionID) {
		if t == origin {
			continue
		}
		t.mirrorUpdate(contenoxSessionID, notif)
	}
}
