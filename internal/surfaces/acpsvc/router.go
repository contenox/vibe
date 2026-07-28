package acpsvc

import (
	"context"
	"errors"
	"sync"

	"github.com/contenox/beam/libacp"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// ErrNoBoundSession reports that no live ACP transport owns the contenox
// session named in the request context. AskApproval's caller falls back to
// the approval-API path instead of hanging on an unanswerable prompt.
var ErrNoBoundSession = errors.New("acpsvc: no ACP transport bound to contenox session")

// SessionRouter maps a contenox session id to the ACP Transport that owns it.
// serve runs many WebSocket connections, each its own Transport, behind one
// shared engine; AskApproval and DeliverToContenoxSession consult this router
// to reach the connection currently hosting a session. Safe for concurrent
// use. The stdio ACP path leaves Deps.SessionRouter nil: it has exactly one
// transport and needs no routing.
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
