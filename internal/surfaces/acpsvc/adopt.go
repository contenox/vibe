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

// Adopt binds an already-running instance+session (typically from a fleet
// dispatch, which attaches no viewer and so has no permission controller) to
// a new upstream session, via Manager.Attach.
//
// It rides session/new's `_meta` (the sanctioned extension point) beside the
// existing contenox.agent key, rather than a new method — see NewSession.
//
// Known limits: replay is bounded by the instance's journal ring, so very
// long pre-adoption history may already be evicted. Dispatch writes nothing
// to chatservice, so the durable transcript starts at adoption; turns before
// it live only in the journal and are lost once the instance stops. The
// dispatch-time cwd governs the running agent; this session/new's cwd only
// affects the upstream session's own bookkeeping. session/delete stops the
// instance (affecting every adopter), matching delete semantics elsewhere.

// AdoptMetaKey is the session/new `_meta` key naming an already-running
// instance + downstream session to bind to instead of creating a new one:
//
//	_meta: { "contenox.adopt": { "instanceId": "<id>", "sessionId": "<downstream acp session id>" } }
//
// Absent or malformed falls back to the historical routing. The same key is
// echoed on the response with the outcome (adoptResult):
//
//	_meta: { "contenox.agent": "<name>",
//	         "contenox.adopt": { "instanceId": ..., "sessionId": ..., "controller": <bool> } }
//
// controller reports whether this connection took control (Attach's
// controllerGranted), so the client can label it without a second round trip.
const AdoptMetaKey = "contenox.adopt"

// adoptRef is the decoded AdoptMetaKey value. Both ids are required — an
// instance multiplexes many sessions, so naming only the instance would be
// ambiguous.
type adoptRef struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
}

// parseAdoptMeta extracts AdoptMetaKey from a request `_meta`. Any missing
// key, malformed json, or blank id reads as ok=false rather than an error, so
// unrelated or future `_meta` falls back to the existing routing.
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

// adoptMetaJSON builds the request-side `{"contenox.adopt": {...}}` object,
// the single definition of that wire shape (symmetrical with parseAdoptMeta).
func adoptMetaJSON(instanceID string, sessionID libacp.SessionID) json.RawMessage {
	return mustJSON(map[string]any{
		AdoptMetaKey: adoptRef{InstanceID: instanceID, SessionID: string(sessionID)},
	})
}

// adoptResult is the response-side contenox.adopt outcome. Control is
// decided by the kernel (Attach's controllerGranted: first viewer of an
// unattended session controls it, a later adopter observes), so the client
// must be told rather than assume.
type adoptResult struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
	Controller bool   `json:"controller"`
}

// adoptedSessionMetaJSON builds an adopted session's response `_meta`:
// ordinary contenox.agent attribution plus the contenox.adopt outcome,
// leaving contenox.agent's existing readers unaffected.
func adoptedSessionMetaJSON(agentName, instanceID string, sessionID libacp.SessionID, controller bool) json.RawMessage {
	return mustJSON(map[string]any{
		AgentMetaKey: agentName,
		AdoptMetaKey: adoptResult{InstanceID: instanceID, SessionID: string(sessionID), Controller: controller},
	})
}

// parseAdoptResultMeta decodes the response-side contenox.adopt outcome, the
// counterpart of parseAdoptMeta. Same defensive contract: malformed or
// missing reads as ok=false.
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

// resolveAdoptTarget validates that ref names a session it is legitimate to
// attach a viewer to, returning the instance's status (whose AgentName
// becomes the session's attribution). Every rejection is InvalidParams, never
// an opaque internal error.
func (t *Transport) resolveAdoptTarget(ctx context.Context, ref adoptRef) (agentinstance.InstanceStatus, error) {
	_ = ctx
	// Stdio connections own their own subprocess; there is no Manager-held
	// instance to adopt.
	if t.deps.Instances == nil {
		return agentinstance.InstanceStatus{}, libacp.NewError(libacp.ErrInvalidParams,
			"contenox.adopt requires the runtime's agent-instance manager (serve); this connection owns its own agent process")
	}

	st, err := t.deps.Instances.Get(ref.InstanceID)
	if err != nil {
		if errors.Is(err, agentinstance.ErrNotFound) {
			return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
				"contenox.adopt: unknown instance %q", ref.InstanceID)
		}
		return agentinstance.InstanceStatus{}, libacp.InternalError(
			fmt.Sprintf("acpsvc: resolve instance %q for adopt: %v", ref.InstanceID, err))
	}
	// Only a running instance has a live downstream to observe or steer.
	if st.State != agentinstance.StateRunning {
		return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.adopt: instance %q is %s, not running; only a running instance can be adopted", ref.InstanceID, st.State)
	}

	// SessionIDs is the kernel's set of open sessions (seeded at OpenSession,
	// dropped at CloseSession), not the viewer hub — so a session that has
	// emitted nothing yet is still adoptable. This rejects only sessions
	// foreign to the instance, not merely quiet ones.
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

// newAdoptedSession mints an upstream session bound to an already-running
// instance+session and attaches a fresh viewer. Sibling of bringUpExternal,
// not a variant: nothing is spawned, and a failure here must never stop the
// instance — it belongs to someone else's dispatch.
//
// Controller status is decided by the kernel (Attach makes the first viewer
// of a controller-less session its controller); this function only reports
// the outcome via adoptedSessionMetaJSON.
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

	// Attribution comes from the kernel, never the client: a client-supplied
	// name could mislabel the session and misdirect a post-reconnect prompt.
	agentName := st.AgentName

	// Mint the upstream session through the ordinary path so it's first-class
	// for session/list, session/load, and the sidebar.
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

	// bound=false: the response isn't on the wire yet, so the downstream's
	// command menu / config pickers are cached and flushed by markBound.
	bridge := newExternalBridge(t, sessionID, false)
	bridge.setDownstreamID(downstreamID)
	// Bind before reading the surface: config options are instance-owned, and
	// the bridge needs to know which instance it views first.
	bridge.bindInstance(t.deps.Instances, ref.InstanceID)
	bridge.persistConfigOptions(ctx, bridge.configOptionsSurface())

	// Hold, don't suppress: unlike a reconnect (whose durable transcript
	// already covers the pre-drop turns), an adopted session has no durable
	// transcript — the in-memory journal is its only history. Attach replays
	// it synchronously, before this response reaches the client, so the
	// replay is queued and flushed by releaseRelay after the response goes out
	// (the client can't resolve a session id it hasn't learned yet).
	bridge.holdRelay()

	granted, attachErr := t.deps.Instances.Attach(ctx, ref.InstanceID, downstreamID, bridge)
	if attachErr != nil {
		// The instance isn't ours to stop; just drop the bridge's relay binding.
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
			// conn/handle stay nil: the kernel owns the connection/process here.
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
	// Persist the kernel's agent name and the instance/downstream ids so a
	// later session/load re-attaches through the ordinary path — adoption is
	// a one-time binding, not a mode the session stays in.
	t.persistSessionAgent(ctx, store, sessionID, agentName)
	t.persistSessionInstance(ctx, sessionID, ref.InstanceID)
	t.persistSessionDownstream(ctx, sessionID, downstreamID)
	t.clearToolCallState(sessionID)

	libacp.AfterResponse(ctx, func() {
		// Journal backlog first (it is the session's history), then the
		// toolbar flush.
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
		Meta:          adoptedSessionMetaJSON(agentName, ref.InstanceID, downstreamID, granted),
	}, nil
}
