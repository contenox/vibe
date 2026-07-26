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
// session named in the request context. serve's shared AskApproval keys its
// fallback on this: when the router cannot route (a headless/API caller, or a
// session with no live WS connection), the engine's HITL request goes to the
// approval-API path instead of hanging on a permission prompt nobody answers.
var ErrNoBoundSession = errors.New("acpsvc: no ACP transport bound to contenox session")

// SessionRouter maps a contenox session id to the ACP Transport that owns
// it. serve runs many ACP WebSocket connections (each its own Transport)
// behind a SINGLE shared process, so anything that must reach "the connection
// hosting session X" cannot close over a single transport the way the stdio ACP
// path does (acp_cmd.go late-binds its lone transport directly). Instead each
// Transport registers its live (contenox session -> this transport) bindings
// here, and the two out-of-band paths into a session consult the router:
//
//   - AskApproval dispatches session/request_permission to the exact WS
//     connection whose client raised the gated tool call — the one beam's
//     PermissionGate is waiting on.
//   - DeliverToContenoxSession pushes a mission report onto the session that
//     FIRED the mission, which is what makes the supervision edge close on serve
//     rather than falling to the operator inbox (see that method).
//
// The stdio ACP path leaves Deps.SessionRouter nil: it has exactly one
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

// bind records that t owns the given contenox session. A nil receiver (the
// stdio path, which passes no router) and empty inputs are no-ops so callers
// need no guard. Last writer wins: if the same session is loaded on a second
// connection, the newer transport becomes the routing target, matching the
// per-transport contenoxToACPID map's own last-writer-wins semantics.
func (r *SessionRouter) bind(contenoxSessionID string, t *Transport) {
	if r == nil || contenoxSessionID == "" || t == nil {
		return
	}
	r.mu.Lock()
	r.bindings[contenoxSessionID] = t
	r.mu.Unlock()
}

// unbind drops the binding for contenoxSessionID, but only when it still points
// at t. Guarding on identity means a transport tearing down a session that a
// newer connection has since re-bound does not evict the live binding.
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

// AskApproval bridges an engine HITL request to the ACP transport that owns the
// contenox session named in ctx (runtimetypes.SessionIDContextKey), driving that
// connection's session/request_permission flow. It returns ErrNoBoundSession
// when no live transport owns the session so the caller can fall back to a
// non-ACP approval path; a genuine deny resolves as (false, nil) and a client
// cancellation as (false, context.Canceled) — neither is ErrNoBoundSession, so
// neither triggers a fallback.
func (r *SessionRouter) AskApproval(ctx context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	contenoxSessionID, _ := ctx.Value(runtimetypes.SessionIDContextKey).(string)
	t, ok := r.transportFor(contenoxSessionID)
	if !ok {
		return false, ErrNoBoundSession
	}
	return t.AskApproval(ctx, req)
}

// DeliverToContenoxSession routes an out-of-band update — a mission report the
// report router is delivering on the supervision edge — to whichever live WS
// connection owns contenoxSessionID, and returns ErrSessionNotLive when none
// does.
//
// This is serve's half of what the in-process editor already had. `/mission`
// sets the mission's ParentSessionID to the FIRING chat session's contenox id
// (handleMission), but serve's report router was given only the agent-instance
// kernel as its deliverer — and the kernel knows unit sessions, never a beam
// chat session. Every report from a beam-fired mission therefore missed and was
// inboxed as "parent gone" while the operator sat watching the very session that
// fired it. Routing through here is what closes that edge: same key, same
// bind/unbind lifecycle as the approval path above, so a report reaches the
// session for exactly as long as a connection holds it.
func (r *SessionRouter) DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error {
	t, ok := r.transportFor(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	return t.DeliverToContenoxSession(ctx, contenoxSessionID, n)
}
