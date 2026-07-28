package agentinstance

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/contenox/beam/libacp"
)

// Viewer is a consumer attached to one downstream session: it receives the
// session's streamed updates via Deliver and, when it is the session's
// controller, answers the downstream agent's permission requests via
// RequestPermission.
type Viewer interface {
	// ID uniquely identifies this viewer within a session; two viewers on the
	// same session must not share an ID.
	ID() string

	// Deliver receives one session update, in order: the replayed journal
	// backlog on attach, then every live update.
	//
	// It must not block — it runs on the fan-out path under the session
	// lock, so a blocking call stalls every other viewer and the downstream
	// read loop. Enqueue and return. The returned error is advisory only.
	Deliver(ctx context.Context, n libacp.SessionNotification) error

	// RequestPermission answers the downstream agent's
	// session/request_permission. Called only on the session's controller
	// viewer (an observer may later be promoted to controller on detach).
	// Unlike Deliver it runs on its own goroutine and may block awaiting a
	// decision.
	RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)
}

// TerminalServer is an optional capability a Viewer may implement to service
// a downstream agent's terminal/* callbacks for the session it controls. It
// is routed only to the session's controller viewer; a controller that does
// not implement it, or a session with no controller, answers terminal/* with
// MethodNotFound. Callbacks run on their own goroutine and may block.
type TerminalServer interface {
	CreateTerminal(ctx context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error)
	TerminalOutput(ctx context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error)
	WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error)
	KillTerminal(ctx context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error)
	ReleaseTerminal(ctx context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error)
}

// sessionState is the per-session viewer set, controller, and replay journal.
type sessionState struct {
	journal      *journal
	viewers      map[string]Viewer
	order        []string // attach order, for deterministic controller promotion
	controllerID string   // "" means no controller (permission falls back to deny)
}

// viewerHub is the instance's per-session registry: journal + fan-out +
// controller routing. All access is serialized by mu, so fan-out and attach
// never interleave — a viewer sees an update exactly once, in its replayed
// backlog or live, never both.
type viewerHub struct {
	instanceID  string
	journalSize int

	// onAttach/onDetach are lifecycle hooks fired outside mu, so a callback
	// into the Manager cannot deadlock the fan-out.
	onAttach func(sessionID libacp.SessionID, viewerID string, controller bool)
	onDetach func(sessionID libacp.SessionID, viewerID string)
	// onUnsupervisedDeny fires when an unattended permission request is
	// refused (see requestPermission). Passive audit only; fires outside mu.
	onUnsupervisedDeny func(sessionID libacp.SessionID)
	// onUnsupervisedRequest, when set, answers a permission request reaching
	// a session with no controller, in place of the built-in deny (see
	// Manager.WithPermissionFallback). Nil keeps the built-in deny.
	onUnsupervisedRequest func(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)

	mu       sync.Mutex
	sessions map[libacp.SessionID]*sessionState
}

func newViewerHub(instanceID string, journalSize int) *viewerHub {
	return &viewerHub{
		instanceID:  instanceID,
		journalSize: journalSize,
		sessions:    make(map[libacp.SessionID]*sessionState),
	}
}

// session returns the state for id, creating it on first use. Callers hold mu.
func (h *viewerHub) session(id libacp.SessionID) *sessionState {
	s := h.sessions[id]
	if s == nil {
		s = &sessionState{
			journal: newJournal(h.journalSize),
			viewers: make(map[string]Viewer),
		}
		h.sessions[id] = s
	}
	return s
}

// deliver journals n and fans it out to every viewer of n.SessionID, in
// arrival order. Runs inline on the ACP read loop, so it relies on Deliver's
// non-blocking contract.
func (h *viewerHub) deliver(ctx context.Context, n libacp.SessionNotification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.session(n.SessionID)
	s.journal.append(n)
	for _, id := range s.order {
		_ = s.viewers[id].Deliver(ctx, n)
	}
}

// attach registers viewer against sessionID, replays that session's journal
// to it, then joins the live fan-out — all under mu so no update can slip
// between replay and join. The first viewer of a session with no controller
// becomes the controller; a duplicate viewer id is rejected.
func (h *viewerHub) attach(ctx context.Context, sessionID libacp.SessionID, viewer Viewer) (controllerGranted bool, err error) {
	vid := viewer.ID()
	if vid == "" {
		return false, fmt.Errorf("agentinstance: viewer ID is required")
	}

	h.mu.Lock()
	s := h.session(sessionID)
	if _, dup := s.viewers[vid]; dup {
		h.mu.Unlock()
		return false, fmt.Errorf("agentinstance: viewer %q already attached to session %q", vid, sessionID)
	}
	s.viewers[vid] = viewer
	s.order = append(s.order, vid)
	if s.controllerID == "" {
		s.controllerID = vid
		controllerGranted = true
	}
	// Replay under the lock so a concurrent live update waits and lands
	// strictly after it.
	for _, n := range s.journal.snapshot() {
		_ = viewer.Deliver(ctx, n)
	}
	h.mu.Unlock()

	if h.onAttach != nil {
		h.onAttach(sessionID, vid, controllerGranted)
	}
	return controllerGranted, nil
}

// detach removes viewer vid from sessionID's fan-out. If it was the
// controller and other viewers remain, the earliest-attached survivor is
// promoted; if none remain the session is dropped. Detaching an unknown
// viewer/session returns an error the caller may ignore.
func (h *viewerHub) detach(sessionID libacp.SessionID, viewerID string) error {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return fmt.Errorf("agentinstance: session %q has no attached viewers", sessionID)
	}
	if _, ok := s.viewers[viewerID]; !ok {
		h.mu.Unlock()
		return fmt.Errorf("agentinstance: viewer %q not attached to session %q", viewerID, sessionID)
	}
	delete(s.viewers, viewerID)
	for i, id := range s.order {
		if id == viewerID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	// Promote on controller departure so a permission request still has an
	// answerer. Control is bound to attachment, not a time lease.
	if s.controllerID == viewerID {
		if len(s.order) > 0 {
			s.controllerID = s.order[0]
		} else {
			s.controllerID = ""
		}
	}
	if len(s.viewers) == 0 {
		delete(h.sessions, sessionID)
	}
	h.mu.Unlock()

	if h.onDetach != nil {
		h.onDetach(sessionID, viewerID)
	}
	return nil
}

// requestPermission routes a downstream session/request_permission to the
// session's controller, releasing mu before calling in (the answer may block
// awaiting a human).
//
// With no controller attached, it defers to the injected fallback if one is
// wired (see Manager.WithPermissionFallback); otherwise, and if the fallback
// itself errors, it denies by default (returned as "cancelled", not a
// JSON-RPC error) — an instance running headless never lets a
// permission-gated action proceed unwatched.
func (h *viewerHub) requestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	h.mu.Lock()
	var controller Viewer
	if s := h.sessions[req.SessionID]; s != nil && s.controllerID != "" {
		controller = s.viewers[s.controllerID]
	}
	h.mu.Unlock()

	if controller == nil {
		// Read outside the lock: the fallback is set once at construction
		// and may block a long time.
		if answer := h.onUnsupervisedRequest; answer != nil {
			resp, err := answer(ctx, req)
			if err == nil {
				if permissionRefused(req, resp) {
					h.reportUnsupervisedDeny(req.SessionID)
				}
				return resp, nil
			}
			// fall through to the built-in deny
		}
		h.reportUnsupervisedDeny(req.SessionID)
		return libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
		}, nil
	}
	return controller.RequestPermission(ctx, req)
}

// reportUnsupervisedDeny fires the passive audit hook for a refused
// unattended request; it only records the outcome, never changes it.
func (h *viewerHub) reportUnsupervisedDeny(sessionID libacp.SessionID) {
	if h.onUnsupervisedDeny != nil {
		h.onUnsupervisedDeny(sessionID)
	}
}

// permissionRefused reports whether resp refuses req: no option selected, or
// the selected option is one of the downstream's own reject options. An
// unknown option id is treated as a refusal — the safe default for an audit
// trail.
func permissionRefused(req libacp.RequestPermissionRequest, resp libacp.RequestPermissionResponse) bool {
	if resp.Outcome.Outcome != libacp.PermissionOutcomeSelected {
		return true
	}
	for _, opt := range req.Options {
		if opt.OptionID != resp.Outcome.OptionID {
			continue
		}
		return opt.Kind != libacp.PermissionAllowOnce && opt.Kind != libacp.PermissionAllowAlways
	}
	return true
}

// terminalServer returns sessionID's controller viewer cast to
// TerminalServer, or nil if there is no controller or it doesn't implement
// the interface. Releases mu before the caller invokes it, since a terminal
// callback may block.
func (h *viewerHub) terminalServer(sessionID libacp.SessionID) TerminalServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[sessionID]
	if s == nil || s.controllerID == "" {
		return nil
	}
	if ts, ok := s.viewers[s.controllerID].(TerminalServer); ok {
		return ts
	}
	return nil
}

// closeSession removes sessionID's entire state as one wholesale teardown,
// unlike detach which removes one viewer at a time. onDetach fires for each
// viewer that was attached, outside the lock. No-op for an unknown session.
func (h *viewerHub) closeSession(sessionID libacp.SessionID) {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return
	}
	ids := append([]string(nil), s.order...)
	delete(h.sessions, sessionID)
	h.mu.Unlock()

	if h.onDetach != nil {
		for _, id := range ids {
			h.onDetach(sessionID, id)
		}
	}
}

// viewerCount reports the total attached viewers across every session. It
// does not report a session count: sessions are materialized lazily here, so
// an open-but-silent session would be invisible — session count comes from
// the driver instead (sessionDriver.sessionIDs).
func (h *viewerHub) viewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	viewers := 0
	for _, s := range h.sessions {
		viewers += len(s.viewers)
	}
	return viewers
}

// journalSnapshot returns a copy of every downstream update retained in
// sessionID's replay journal, in arrival order, uninterpreted (see agentText
// for the text-only view). Nil if the session has no state yet. The journal
// captures updates whether or not a viewer is watching.
func (h *viewerHub) journalSnapshot(sessionID libacp.SessionID) []libacp.SessionNotification {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return nil
	}
	snapshot := s.journal.snapshot()
	h.mu.Unlock()
	return snapshot
}

// agentText concatenates the text of every agent_message_chunk retained in
// sessionID's replay journal, in arrival order. Returns "" if the session has
// no state yet.
func (h *viewerHub) agentText(sessionID libacp.SessionID) string {
	h.mu.Lock()
	s := h.sessions[sessionID]
	if s == nil {
		h.mu.Unlock()
		return ""
	}
	snapshot := s.journal.snapshot()
	h.mu.Unlock()

	var sb strings.Builder
	for _, n := range snapshot {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}
