package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

// ADOPT — binding an ALREADY-RUNNING instance+session to a NEW upstream session.
//
// # The hole this closes
//
// A fleet dispatch (POST /api/fleet/dispatch → fleetservice.Dispatch) brings up an
// instance, opens a downstream ACP session, and fires a first prompt — but it attaches
// NO viewer. The kernel routes a downstream session/request_permission to the session's
// CONTROLLER viewer, and the first viewer to attach becomes that controller
// (agentinstance/viewer.go). With no viewer ever attached, a dispatched session has no
// controller, so its first permission request is auto-denied as unsupervised
// (agentinstance.EventUnsupervisedDeny) and the turn ends in a refusal. Nobody can watch
// it, steer it, cancel it from a chat surface, or answer it.
//
// The kernel already supports the fix: Manager.Attach replays a session's journal to a
// new viewer, joins it to the live fan-out, and permits N viewers with one controller.
// What was missing is a TRANSPORT VERB meaning "bind this EXISTING instance+session to a
// new upstream session". That verb is adopt, and this file is all of it.
//
// # Why session/new + `_meta` and not a new method
//
// `_meta` namespaced extensions are the sanctioned extension point (see
// docs/development/blueprints/beam/beam-on-acp.md): a conformant client that does not
// recognize the key ignores it, so no protocol method is added and no peer breaks. Adopt
// therefore sits beside the existing contenox.agent key in the SAME session/new routing
// switch — see NewSession.
//
// # Honest limitations (do NOT read this as full fidelity)
//
//   - The replayed history is the journal, and the journal is a BOUNDED RING
//     (agentinstance/journal.go, sized by WithJournalSize). A long-running dispatched
//     session may already have evicted its earliest updates, so adopt replays what the
//     journal STILL HOLDS, not necessarily the whole conversation.
//   - The DURABLE transcript begins AT ADOPTION. Dispatch drives the kernel directly and
//     writes nothing to chatservice, so the turns before adoption were never persisted:
//     closing an adopted session and reloading it shows only post-adoption turns. The
//     in-memory journal is the ONLY record of what came before, and it dies with the
//     instance.
//   - The downstream session's cwd was fixed at dispatch. The cwd on this session/new
//     governs only the UPSTREAM session's own workspace bookkeeping; it does not and
//     cannot re-root the running agent.
//   - An adopted session inherits the external path's teardown asymmetry unchanged: a
//     disconnect or session/close DETACHES (the dispatched instance keeps running), but
//     session/delete STOPS the instance — including for any other adopter of the same
//     session. Deleting an adopted chat therefore ends the dispatched run, which is what
//     "delete" means everywhere else here, but is worth knowing before doing it.

// AdoptMetaKey is the session/new `_meta` key a client uses to ADOPT an already-running
// agent instance + downstream session (typically one created by fleet dispatch) instead
// of creating a new one:
//
//	_meta: { "contenox.adopt": { "instanceId": "<id>", "sessionId": "<downstream acp session id>" } }
//
// It is a contenox extension in the spec's reserved `_meta` namespace, the same precedent
// as AgentMetaKey; a conformant client that does not recognize it ignores `_meta`
// entirely. Absent (or malformed) = the historical routing, byte-for-byte.
//
// The SAME key is echoed on the session/new RESPONSE with the adopt OUTCOME (see adoptResult
// / adoptedSessionMetaJSON), so the response `_meta` of an adopted session is:
//
//	_meta: { "contenox.agent": "<name>",
//	         "contenox.adopt": { "instanceId": ..., "sessionId": ..., "controller": <bool> } }
//
// where controller reports whether this connection took control (Attach's controllerGranted)
// — the fact beam labels "übernommen" vs "beobachten" from, without a second round trip.
const AdoptMetaKey = "contenox.adopt"

// adoptRef is the decoded AdoptMetaKey value: which running instance, and which of ITS
// downstream sessions, this session/new is binding to. Both ids are required — an adopt
// naming only an instance would be ambiguous (an instance multiplexes many sessions over
// one connection).
type adoptRef struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
}

// parseAdoptMeta extracts the AdoptMetaKey value from a request `_meta`. It mirrors
// parseAgentMeta's DEFENSIVE style deliberately: a missing key, malformed json, a
// wrong-shaped value, or either id blank all read as "no adopt" (ok=false), so a client
// shipping unrelated `_meta` — or a client of a future protocol revision — still lands on
// the existing routing rather than failing a session/new it did not mean to change.
func parseAdoptMeta(meta json.RawMessage) (adoptRef, bool) {
	if len(meta) == 0 {
		return adoptRef{}, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return adoptRef{}, false
	}
	raw, ok := m[AdoptMetaKey]
	if !ok {
		return adoptRef{}, false
	}
	var ref adoptRef
	if json.Unmarshal(raw, &ref) != nil {
		return adoptRef{}, false
	}
	ref.InstanceID = strings.TrimSpace(ref.InstanceID)
	ref.SessionID = strings.TrimSpace(ref.SessionID)
	if ref.InstanceID == "" || ref.SessionID == "" {
		return adoptRef{}, false
	}
	return ref, true
}

// adoptMetaJSON builds the `{"contenox.adopt": {...}}` object a client sends on
// session/new to adopt. It is the single definition of the REQUEST wire shape — beam's
// fleet board and a future `contenox fleet attach` both mirror THIS, and it keeps the
// encode and the decode (parseAdoptMeta) provably symmetrical.
func adoptMetaJSON(instanceID string, sessionID libacp.SessionID) json.RawMessage {
	return mustJSON(map[string]any{
		AdoptMetaKey: adoptRef{InstanceID: instanceID, SessionID: string(sessionID)},
	})
}

// adoptResult is the RESPONSE-side value of the contenox.adopt `_meta` key: the OUTCOME of
// an adopt, echoed on the session/new response so the adopting client learns what it bound
// to and — the one fact it cannot compute for itself — whether it became the session's
// CONTROLLER. The kernel decides control (Attach's controllerGranted: the first viewer of
// an unattended dispatched session takes control; a later adopter of a session that already
// has one joins as an OBSERVER), so the client must be TOLD rather than assume. The UI
// labels the two outcomes "übernommen" (Controller true — took control, answers the unit's
// permission asks) vs "beobachten" (false — watching only). InstanceID/SessionID echo the
// request's adoptRef so a client can confirm the exact binding it asked for.
type adoptResult struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
	Controller bool   `json:"controller"`
}

// adoptedSessionMetaJSON builds the session/new RESPONSE `_meta` for an adopted session:
// the ordinary contenox.agent attribution PLUS the contenox.adopt OUTCOME (adoptResult). It
// is the single definition of the adopted-response wire shape — beam reads
// _meta["contenox.adopt"].controller off it to label the tab, and confirms the binding from
// instanceId/sessionId. It deliberately leaves contenox.agent unchanged so every existing
// reader (parseAgentMeta, session/list attribution) is untouched by the added key.
func adoptedSessionMetaJSON(agentName, instanceID string, sessionID libacp.SessionID, controller bool) json.RawMessage {
	return mustJSON(map[string]any{
		AgentMetaKey: agentName,
		AdoptMetaKey: adoptResult{InstanceID: instanceID, SessionID: string(sessionID), Controller: controller},
	})
}

// parseAdoptResultMeta decodes the RESPONSE-side contenox.adopt outcome from a session/new
// response `_meta`. It is the counterpart of parseAdoptMeta (which decodes the REQUEST) and
// exists so the encode here and the beam decode are provably symmetrical, and so a test can
// pin the wire shape. Same defensive contract as parseAdoptMeta: any missing / malformed
// shape reads as ok=false rather than an error, so a client of a future protocol revision
// still degrades to "no adopt outcome reported" instead of failing.
func parseAdoptResultMeta(meta json.RawMessage) (adoptResult, bool) {
	if len(meta) == 0 {
		return adoptResult{}, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return adoptResult{}, false
	}
	raw, ok := m[AdoptMetaKey]
	if !ok {
		return adoptResult{}, false
	}
	var r adoptResult
	if json.Unmarshal(raw, &r) != nil {
		return adoptResult{}, false
	}
	return r, true
}

// resolveAdoptTarget validates that ref names a session it is legitimate to attach a
// viewer to, returning the instance's status (whose AgentName is the session's
// attribution — see newAdoptedSession). Every rejection is an InvalidParams the client can
// act on, never an opaque internal error.
//
// ctx is taken and an error returned on every seam per the package's convention; the
// kernel's Get is purely in-memory today, so ctx governs nothing here yet — it is the
// seam's shape, not a promise that this call is cheap forever.
func (t *Transport) resolveAdoptTarget(ctx context.Context, ref adoptRef) (agentinstance.InstanceStatus, error) {
	_ = ctx
	// 1. Adopt is meaningless without the runtime's instance kernel. On the stdio path
	// (`contenox acp`, Deps.Instances nil) the driver OWNS its subprocess bound to the
	// connection — there is no Manager-held instance to adopt and no fleet to dispatch
	// one. Refuse plainly rather than silently falling through to a fresh bring-up,
	// which would spawn a SECOND agent and quietly ignore what the client asked for.
	if t.deps.Instances == nil {
		return agentinstance.InstanceStatus{}, libacp.NewError(libacp.ErrInvalidParams,
			"contenox.adopt requires the runtime's agent-instance manager (serve); this connection owns its own agent process")
	}

	// 2. The instance must exist and be RUNNING. A stopped/errored/warning instance has
	// no live downstream to observe or steer, so attaching to it would produce a session
	// that looks alive and answers nothing.
	st, err := t.deps.Instances.Get(ref.InstanceID)
	if err != nil {
		if errors.Is(err, agentinstance.ErrNotFound) {
			return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
				"contenox.adopt: unknown instance %q", ref.InstanceID)
		}
		return agentinstance.InstanceStatus{}, libacp.InternalError(
			fmt.Sprintf("acpsvc: resolve instance %q for adopt: %v", ref.InstanceID, err))
	}
	if st.State != agentinstance.StateRunning {
		return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.adopt: instance %q is %s, not running; only a running instance can be adopted", ref.InstanceID, st.State)
	}

	// 3. The session must belong to THIS instance. Without this check a client could name
	// an arbitrary session id and Attach would happily mint fresh viewer state for it —
	// making the caller the controller of a session it has no relationship to, and
	// (worse) of one that does not exist, which silently swallows its permission
	// requests forever.
	//
	// InstanceStatus.SessionIDs is the kernel's set of OPEN sessions, sourced from the
	// session driver (seeded at OpenSession, dropped at CloseSession) — not from the
	// viewer hub. That distinction is load-bearing here: a dispatched session that has
	// emitted NOTHING yet is still open and still adoptable, and on local inference the
	// silent window (cold model load, long reasoning) is exactly when an operator most
	// wants to take control. This check therefore rejects only genuinely foreign session
	// ids, never merely quiet ones.
	if !containsSessionID(st.SessionIDs, ref.SessionID) {
		return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.adopt: session %q is not live on instance %q", ref.SessionID, ref.InstanceID)
	}
	return st, nil
}

// containsSessionID reports whether ids holds sid. A linear scan is right: an instance
// multiplexes a handful of sessions, and SessionIDs is a fresh sorted snapshot per call.
func containsSessionID(ids []string, sid string) bool {
	for _, id := range ids {
		if id == sid {
			return true
		}
	}
	return false
}

// newAdoptedSession mints an upstream contenox session bound to an ALREADY-RUNNING
// instance+session and attaches a fresh viewer to it. It is the sibling of the
// bringUpExternal path in NewSession, NOT a variant of it: nothing is spawned, no
// downstream session/new is driven, and a failure here must never STOP the instance —
// the instance is someone else's (a dispatch's), and a rejected adopt has to leave it
// exactly as it found it.
//
// It reports the change through the caller's tracker span (reportChange), so a
// session/new that adopts is recorded on the same span as one that spawns.
//
// # Controller semantics
//
// None are DECIDED here on purpose — the kernel already owns that. Attach makes the first
// viewer of a controller-less session its controller, so adopting a dispatched session
// (which by construction has none) hands this connection the permission answers: the whole
// point. If another beam tab adopted first, the kernel makes this viewer an OBSERVER and
// reports controllerGranted=false. Both outcomes are correct and neither is fought over
// here — this function only REPORTS which one happened, threading granted into the response
// `_meta` (adoptedSessionMetaJSON) so the UI can label control vs observation.
func (t *Transport) newAdoptedSession(
	ctx context.Context,
	internalID string,
	sessionID libacp.SessionID,
	sessionCwd string,
	workspaceID string,
	store runtimetypes.Store,
	ref adoptRef,
	reportChange func(string, any),
) (libacp.NewSessionResponse, error) {
	st, err := t.resolveAdoptTarget(ctx, ref)
	if err != nil {
		return libacp.NewSessionResponse{}, err
	}
	downstreamID := libacp.SessionID(ref.SessionID)

	// ATTRIBUTION comes from the kernel, never from the client. The instance knows which
	// declared agent it is running; a client-supplied name could mislabel the session in
	// session/list and, worse, send the next prompt after a reconnect to a DIFFERENT
	// agent (markExternalIfPersisted rebuilds the driver from this persisted name).
	agentName := st.AgentName

	// Mint the upstream contenox session exactly as the ordinary paths do, so an adopted
	// session is first-class for session/list, session/load and beam's sidebar rather
	// than a second-class view that vanishes on reload.
	ag := agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: workspaceID,
		Identity:    "acp-client",
	})
	contenoxSessionID, sessErr := ag.SessionNew(ctx, internalID)
	if sessErr != nil {
		return libacp.NewSessionResponse{}, fmt.Errorf("acpsvc: agent.SessionNew: %w", sessErr)
	}

	// Build a fresh per-attachment viewer bridge. bound=false for the same reason the
	// eager session/new bring-up uses it: this response is not on the wire yet, so the
	// downstream's command menu / config pickers are cached and flushed by markBound.
	bridge := newExternalBridge(t, sessionID, false)
	bridge.setDownstreamID(downstreamID)
	// Bind BEFORE reading the surface: the kernel OWNS an instance-backed session's
	// config options (see configOptionsSurface), and the bridge can only read them once
	// it knows which instance it is a viewer of.
	bridge.bindInstance(t.deps.Instances, ref.InstanceID)
	// Persist the kernel's captured surface under THIS upstream session id, mirroring
	// openInstanceSession, so a session/load of the adopted session restores its toolbar
	// (mode/model/downstream pickers) without waiting for a prompt to respawn anything.
	bridge.persistConfigOptions(ctx, bridge.configOptionsSurface())

	// HOLD the relay across Attach — do NOT suppress it.
	//
	// This is the ONE place adopt deliberately differs from the reconnect path
	// (externalDriver.ensureAttached). ensureAttached SUPPRESSES the journal replay
	// because the durable chatservice transcript already replayed those same turns at
	// session/load, so relaying both would double-emit them. An adopted session has NO
	// durable transcript at all — dispatch never wrote one — so the in-memory journal is
	// its ONLY history and dropping it would hand the adopter a blank session. Getting
	// this backwards produces either an empty session (suppress) or a doubled one
	// (replay on the reconnect path); hence hold, not suppress.
	//
	// Holding rather than relaying straight through matters because Attach replays
	// SYNCHRONOUSLY, i.e. BEFORE this session/new response reaches the client — and a
	// client cannot resolve a session id it has not learned yet, so it drops updates for
	// it (the exact reason externalBridge.bound exists). The held queue is flushed in
	// arrival order by releaseRelay, scheduled after the response below.
	bridge.holdRelay()

	granted, attachErr := t.deps.Instances.Attach(ctx, ref.InstanceID, downstreamID, bridge)
	if attachErr != nil {
		// Leave the instance ALONE — it is not ours to stop. Just drop the redundant
		// bridge's relay binding so it stops retaining this transport.
		bridge.detachFrom(t)
		return libacp.NewSessionResponse{}, libacp.InternalError(
			fmt.Sprintf("acpsvc: adopt session %q on instance %q: %v", ref.SessionID, ref.InstanceID, attachErr))
	}

	entry := &sessionEntry{
		WorkspaceID:       workspaceID,
		Cwd:               sessionCwd,
		InternalSessionID: contenoxSessionID,
		HITLPolicy:        hitlPolicyDefaultValue,
		driver: &externalDriver{
			t:          t,
			agentName:  agentName,
			upstreamID: sessionID,
			// conn/handle stay nil: the kernel owns the connection and the process on
			// the Instances path, and adopt only ever runs there.
			instanceID:   ref.InstanceID,
			downstreamID: downstreamID,
			bridge:       bridge,
		},
	}
	t.sessionMu.Lock()
	t.sessions[sessionID] = entry
	t.bindContenoxSession(contenoxSessionID, sessionID)
	t.sessionMu.Unlock()

	t.persistSessionCwd(ctx, store, sessionID, sessionCwd)
	// Persist the KERNEL's agent name so session/list attributes this session correctly
	// and a later reconnect rebuilds an externalDriver for the right agent.
	t.persistSessionAgent(ctx, store, sessionID, agentName)
	// Persist the instance + downstream ids so a later session/load goes down the
	// ORDINARY ensureAttached re-attach path with no adopt-specific logic — adoption is
	// a one-time binding, not a mode the session stays in.
	t.persistSessionInstance(ctx, sessionID, ref.InstanceID)
	t.persistSessionDownstream(ctx, sessionID, downstreamID)
	t.clearToolCallState(sessionID)

	libacp.AfterResponse(ctx, func() {
		// Order: the held journal backlog first (it IS the session's history, and the
		// client can now resolve the session id), then markBound's toolbar flush.
		bridge.releaseRelay(ctx)
		bridge.markBound(ctx)
	})

	reportChange(string(sessionID), map[string]any{
		"contenox_session_id":   contenoxSessionID,
		"workspace_id":          workspaceID,
		"external_agent":        agentName,
		"adopted_instance_id":   ref.InstanceID,
		"adopted_session_id":    ref.SessionID,
		"adopted_as_controller": granted,
	})
	return libacp.NewSessionResponse{
		SessionID:     sessionID,
		ConfigOptions: t.sessionConfigOptions(ctx, entry),
		// The response `_meta` carries the adopt OUTCOME, not just attribution: alongside
		// contenox.agent it echoes contenox.adopt with `controller` set to Attach's granted,
		// so the client can label the surface "übernommen" (took control) vs "beobachten"
		// (observing) the moment the session opens — no second round trip. See
		// adoptedSessionMetaJSON for the wire shape.
		Meta: adoptedSessionMetaJSON(agentName, ref.InstanceID, downstreamID, granted),
	}, nil
}
