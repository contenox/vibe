package acpsvc

import (
	"context"
	"errors"
	"sync"

	"github.com/contenox/contenox/libacp"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// ErrNoBoundSession reports that no live ACP transport owns the contenox
// session named in the request context. AskApproval's caller falls back to
// the approval-API path instead of hanging on an unanswerable prompt.
var ErrNoBoundSession = errors.New("acpsvc: no ACP transport bound to contenox session")

// SessionRouter maps a contenox session id to the ACP Transport that owns it.
// One engine may be driven by several connections at once — serve's WebSockets,
// and the ACP profile's stdio connection alongside every relay attachment — and
// a question raised inside a turn must reach the client that asked for the
// turn, not whichever connection the process happened to bind at startup.
// AskApproval, DeliverToContenoxSession and PromptContenoxSession all address a
// session through this one registry. Safe for concurrent use, and every method
// is nil-safe so a caller serving exactly one connection may leave
// Deps.SessionRouter unset.
//
// # Ownership, and the three cases it has to answer
//
// A transport claims a session when it creates, loads, resumes or adopts it,
// and again on every turn it prompts (Transport.claimSessionRouting). One
// session has exactly one owner.
//
// Nobody attached: the lookup fails with ErrNoBoundSession rather than picking
// an arbitrary connection. The caller decides what an unanswerable question
// means — falling back to another transport, or reporting the durable ask so an
// operator answers it from a terminal — and neither is a decision a routing
// table may make on its own.
//
// Several clients on one session: the most recent claim wins, so the client
// driving the turn owns the questions the turn raises. A card for a turn that
// was already running when another client claimed the session follows the
// claim. That is the cost of a single owner, and it is the cheaper cost: asking
// every attached client would put one question in front of two humans and take
// whichever answered first as the verdict.
//
// The owner detaching: the connection's close releases its entries
// (Transport.releaseSessionRouting), so an outstanding request fails as
// ErrConnectionClosed on the connection that was actually asked, and the next
// request reports ErrNoBoundSession instead of being written to a dead
// transport. Release is identity-guarded, so a session another connection has
// since claimed keeps its live owner.
type SessionRouter struct {
	mu       sync.RWMutex
	bindings map[string]*Transport
}

// NewSessionRouter returns an empty router ready to be shared across a
// serve process's ACP WebSocket transports.
func NewSessionRouter() *SessionRouter {
	return &SessionRouter{bindings: make(map[string]*Transport)}
}

// bind records that t owns contenoxSessionID. A nil receiver or empty input
// is a no-op. Last writer wins, matching the per-transport contenoxToACPID
// map's own semantics.
func (r *SessionRouter) bind(contenoxSessionID string, t *Transport) {
	if r == nil || contenoxSessionID == "" || t == nil {
		return
	}
	r.mu.Lock()
	r.bindings[contenoxSessionID] = t
	r.mu.Unlock()
}

// unbind drops the binding for contenoxSessionID, but only when it still
// points at t: a transport tearing down a session that a newer connection has
// since re-bound must not evict the live binding.
func (r *SessionRouter) unbind(contenoxSessionID string, t *Transport) {
	if r == nil || contenoxSessionID == "" {
		return
	}
	r.mu.Lock()
	if r.bindings[contenoxSessionID] == t {
		delete(r.bindings, contenoxSessionID)
	}
	r.mu.Unlock()
}

func (r *SessionRouter) transportFor(contenoxSessionID string) (*Transport, bool) {
	if r == nil || contenoxSessionID == "" {
		return nil, false
	}
	r.mu.RLock()
	t, ok := r.bindings[contenoxSessionID]
	r.mu.RUnlock()
	return t, ok
}

// AskApproval bridges an engine HITL request to the ACP transport owning the
// contenox session in ctx (runtimetypes.SessionIDContextKey), driving that
// connection's session/request_permission flow. Returns ErrNoBoundSession only
// when no transport owns the session; a genuine deny resolves (false, nil)
// and a client cancellation (false, context.Canceled) — neither triggers the
// caller's fallback.
func (r *SessionRouter) AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	contenoxSessionID, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	t, ok := r.transportFor(contenoxSessionID)
	if !ok {
		return false, ErrNoBoundSession
	}
	return t.AskApproval(ctx, req)
}

// DeliverToContenoxSession routes an out-of-band mission report to whichever
// live WS connection owns contenoxSessionID, returning ErrSessionNotLive when
// none does. Ownership follows the same bind/unbind lifecycle as AskApproval,
// so a report reaches the session for exactly as long as a connection holds it.
func (r *SessionRouter) DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error {
	t, ok := r.transportFor(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	return t.DeliverToContenoxSession(ctx, contenoxSessionID, n)
}
