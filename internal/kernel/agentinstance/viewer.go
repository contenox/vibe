package agentinstance

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/contenox/beam/libacp"
)

// Viewer is a consumer attached to one downstream session of a running
// instance: a viewer both RECEIVES the session's
// streamed updates (Deliver, the fan-out) and — when it is the session's
// controller — ANSWERS the downstream agent's permission requests
// (RequestPermission, a callback a byte-terminal viewer has no equivalent
// of). It is defined HERE and typed only on libacp so an implementer (a future
// acpsvc bridge, beam's live view) needs no import from this package beyond the
// interface, and no import cycle can form.
type Viewer interface {
	// ID uniquely identifies this viewer WITHIN a session; it is the key the
	// hub registers under and the id Detach later names. Two viewers on the same
	// session must not share an ID.
	ID() string

	// Deliver receives one downstream session/update for the session this viewer
	// is attached to, both the REPLAYED journal backlog (on attach) and every
	// subsequent LIVE update, in order.
	//
	// It MUST NOT block. Deliver runs on the instance's fan-out path — the ACP
	// read loop for a live update, or the attaching caller's goroutine for the
	// replay — while the session lock is held, so a blocking Deliver stalls every
	// other viewer of the session AND the downstream read loop itself. Enqueue and
	// return; do the slow work (a WebSocket write, a render) elsewhere. The
	// returned error is advisory (logged by the caller at most); it never
	// disturbs the downstream turn.
	Deliver(ctx context.Context, n libacp.SessionNotification) error

	// RequestPermission answers the downstream agent's
	// session/request_permission. The instance invokes it ONLY on the session's
	// controller viewer; an observer implements it to satisfy the interface but
	// it is never called while that viewer is an observer (it WILL be called if
	// the viewer is later promoted to controller — see the hub's detach
	// promotion). Unlike Deliver it runs on its OWN goroutine (ACP dispatches
	// each inbound request separately), so it MAY block awaiting a human decision.
	RequestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error)
}

// TerminalServer is an OPTIONAL capability a Viewer MAY also implement to service a
// downstream agent's terminal/* client-callback family (create/output/wait/kill/release) for
// the session it controls. The instance's journaling harness routes an inbound terminal/*
// request to the session's CONTROLLER viewer iff that controller implements TerminalServer; a
// controller that does not — or a session with no controller — answers terminal/* with
// MethodNotFound, exactly as an agent that never advertised the terminal client capability
// expects. Like Deliver/RequestPermission the terminal callbacks run on their own dispatched
// goroutines, so WaitForTerminalExit MAY block until the command exits.
//
// It is the second inbound-callback surface (after RequestPermission) the kernel ROUTES to a
// controller: the byte-terminal reference fans bytes one way, but an ACP downstream also calls
// back. The kernel itself has NO shell dependency — it only routes terminal/* to whoever can
// serve it (an acpsvc bridge maps them onto the runtime's shell sessions). Whether the
// downstream is even TOLD terminals exist is governed separately, by SessionSpec.Terminal at
// OpenSession: the capability is advertised only when the consumer says a terminal server may
// attach.
//
// The method set mirrors the terminal subset of libacp.Client, so an implementer that already
// satisfies libacp.Client (e.g. an acpsvc bridge) satisfies this by construction.
type TerminalServer interface {
	CreateTerminal(ctx context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error)
	TerminalOutput(ctx context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error)
	WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error)
	KillTerminal(ctx context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error)
	ReleaseTerminal(ctx context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error)
}

// sessionState is the per-session viewer set, controller, and replay journal —
// the ACP counterpart of a single ProcessPty's writers + cacheBytesBuf, but
// scoped per downstream session rather than per process (one instance multiplexes
// many sessions over one connection).
type sessionState struct {
	journal      *journal
	viewers      map[string]Viewer
	order        []string // attach order, for deterministic controller promotion
	controllerID string   // "" means no controller (permission falls back to deny)
}

// viewerHub is the instance's per-session registry: journal + fan-out + controller
// routing. All access is serialized by mu, so the fan-out (deliver) and a viewer
// attach can never interleave — that mutual exclusion is what makes the replay
// exactly-once and correctly ordered (a viewer either sees an update in its
// replayed backlog OR live, never both, never neither, never out of order).
//
// This tightens the reference, which replayed its byte cache and joined the live
// fan-out without a lock spanning both (an accepted small race for a terminal);
// for structured events we hold the invariant strictly.
type viewerHub struct {
	instanceID  string
	journalSize int

	// onAttach/onDetach are the instance's lifecycle hooks, fired OUTSIDE mu so a
	// sink that calls back into the Manager cannot deadlock the fan-out.
	onAttach func(sessionID libacp.SessionID, viewerID string, controller bool)
	onDetach func(sessionID libacp.SessionID, viewerID string)
	// onUnsupervisedDeny fires when an unattended permission request ends in a
	// REFUSAL (see requestPermission). Passive audit only — like onAttach it fires
	// OUTSIDE mu and never influences the outcome it reports.
	onUnsupervisedDeny func(sessionID libacp.SessionID)
	// onUnsupervisedRequest, when set, ANSWERS a permission request that reached a
	// session with no controller, in place of the built-in deny. It is the hub's
	// half of the Manager's WithPermissionFallback option: the Manager closes the
	// instance's identity over it, so the hub passes only the request. Nil (the
	// default) keeps the built-in deny, so an unwired Manager behaves exactly as it
	// did before this seam existed.
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

// deliver journals n and fans it out to every viewer of n.SessionID, in arrival
// order — the structured-event form of ProcessPty.readInit's write-to-all-writers
// loop. It runs on the ACP read loop (inline, per libacp's notification
// dispatch), so it relies on the non-blocking Deliver contract.
func (h *viewerHub) deliver(ctx context.Context, n libacp.SessionNotification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.session(n.SessionID)
	s.journal.append(n)
	for _, id := range s.order {
		_ = s.viewers[id].Deliver(ctx, n)
	}
}

// attach registers viewer against sessionID, REPLAYS that session's journal to it
// (ProcessPty.ReadCache), then leaves it in the live fan-out — all under mu so no
// update can slip between replay and join. The first viewer of a session with no
// controller becomes the controller (controllerGranted true); later viewers are
// observers. A viewer id already attached to the session is rejected (mirrors the
// reference's "connection already exists").
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
	// Replay the backlog under the lock, so a concurrent live update (which also
	// needs mu) is forced to wait and therefore lands strictly AFTER the replay.
	for _, n := range s.journal.snapshot() {
		_ = viewer.Deliver(ctx, n)
	}
	h.mu.Unlock()

	if h.onAttach != nil {
		h.onAttach(sessionID, vid, controllerGranted)
	}
	return controllerGranted, nil
}

// detach removes viewer vid from sessionID's fan-out. If it was the controller
// and other viewers remain, the earliest-attached survivor is promoted (so a
// session keeps a controller across a controller's departure); if none remain
// the session is dropped entirely. Detaching an unknown viewer/session is an
// error the caller may ignore.
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
	// Promote on controller departure: the next-oldest viewer takes control, so a
	// permission request still has an answerer. Control is bound to attachment,
	// not a wall-clock lease — see the package doc's divergence note.
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
// session's controller. It reads the controller under mu then RELEASES the lock
// before calling into it, because the controller's answer may block awaiting a
// human — holding mu there would stall the whole session's fan-out.
//
// Fallback (no controller attached): the INJECTED answerer when one is wired
// (onUnsupervisedRequest — see Manager.WithPermissionFallback), otherwise an
// UNSUPERVISED DENY returned as a spec-graceful "cancelled" outcome. Denying
// remains the built-in default for an instance running headless — nothing gets to
// perform a permission-gated action with no one watching — and cancelled (rather
// than a JSON-RPC error) lets the downstream turn end cleanly instead of faulting.
//
// The kernel knows NOTHING about what a wired fallback does: it hands over the
// request and returns whatever comes back. Approvals, envelopes and inboxes are
// service-layer judgments (see the package doc's policy-free invariant); this seam
// exists so they can be made without any of them reaching down here.
//
// A fallback that ERRORS falls back to the built-in deny rather than faulting the
// downstream turn: an answerer that could not decide must not be more disruptive
// than having no answerer at all.
func (h *viewerHub) requestPermission(ctx context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	h.mu.Lock()
	var controller Viewer
	if s := h.sessions[req.SessionID]; s != nil && s.controllerID != "" {
		controller = s.viewers[s.controllerID]
	}
	h.mu.Unlock()

	if controller == nil {
		// Read the fallback under no lock: it is set once at construction and may
		// block for a long time (a durable ask awaiting a human), so holding mu
		// across it would stall the whole session's fan-out.
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

// reportUnsupervisedDeny fires the passive audit hook for an unattended request
// that was REFUSED. The decision is already made (and the lock released); the hook
// only records it and never changes the outcome.
func (h *viewerHub) reportUnsupervisedDeny(sessionID libacp.SessionID) {
	if h.onUnsupervisedDeny != nil {
		h.onUnsupervisedDeny(sessionID)
	}
}

// permissionRefused reports whether resp REFUSES req — either because it selected
// no option at all (cancelled, the built-in headless outcome) or because the option
// it selected is one of the downstream's own REJECT options. It is what keeps the
// unsupervised-deny audit event truthful once a fallback can also PERMIT: an
// allow_once selected by an envelope is not a deny and must not be recorded as one.
//
// An unknown option id is treated as a refusal: the kernel cannot verify that an id
// it never saw offered grants anything, and over-reporting a refusal is the safe
// direction for an audit trail.
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

// terminalServer returns sessionID's controller viewer cast to TerminalServer, or nil when
// there is no controller or the controller does not implement it. It reads the controller
// under mu then RELEASES the lock before the caller invokes it — a terminal callback
// (WaitForTerminalExit especially) may block, and holding mu there would stall the session's
// fan-out. Mirrors requestPermission's controller lookup, for the terminal/* callback family.
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

// closeSession removes sessionID's ENTIRE state — its journal, its viewer registry, and its
// controller — as one wholesale teardown (the kernel's CloseSession), unlike detach which
// removes one viewer at a time. onDetach fires for each viewer that was still attached,
// OUTSIDE the lock, so an event sink sees a departure for every viewer the closed session
// held. A no-op for an unknown session.
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

// viewerCount reports the total attached viewers across every session — the WATCHER
// half of InstanceStatus, and the only status fact the hub is authoritative for.
//
// It deliberately does not also report a session count. The hub materializes a
// session's state lazily (on its first delivered update or first attach — see
// session), so its session set answers "who is being watched", not "what is open";
// the latter comes from the driver, which OpenSession seeds and CloseSession drops
// (sessionDriver.sessionIDs). Reporting a session count from here once meant an open
// but silent session was invisible in InstanceStatus.
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
// sessionID's replay journal, in arrival order — the RAW counterpart of
// agentText. Where agentText interprets the journal down to the unit's words,
// this returns it uninterpreted, so a consumer that needs a different slice of
// it (the attention layer folds the tool-call updates for changed-files and
// scope) does its own interpretation and the kernel stays policy-free. A session
// with no state yet yields nil. Held under mu like every other journal access;
// the journal captures every downstream update whether or not a viewer is
// watching, which is why a dispatched, unwatched unit's diffs are recoverable
// here at all.
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
// sessionID's replay journal, in arrival order — the unit's own words as they
// streamed. It is the READ side of the journal the hub already keeps for replay:
// a session with no state yet (nothing delivered, no viewer ever attached) yields
// "". Held under mu like every other journal access, and it interprets nothing
// beyond "which updates are agent text", so it stays as policy-free as the journal
// it reads. The journal captures every downstream update whether or not a viewer
// is watching (deliver journals unconditionally), which is exactly why a
// dispatched, unwatched unit's words are recoverable here at all.
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
