package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/kernel/agentinstance"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

// AdoptMetaKey is the session/new `_meta` key naming an already-running instance
// and downstream session to bind to instead of creating a new one:
//
//	_meta: { "contenox.adopt": { "instanceId": "<id>", "sessionId": "<id>" } }
//
// Absent or malformed falls back to the historical routing. The key is echoed on
// the response with the outcome, including whether this connection took
// control.
const AdoptMetaKey = "contenox.adopt"

type adoptRef struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
}

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

func adoptMetaJSON(instanceID string, sessionID libacp.SessionID) json.RawMessage {
	return mustJSON(map[string]any{
		AdoptMetaKey: adoptRef{InstanceID: instanceID, SessionID: string(sessionID)},
	})
}

type adoptResult struct {
	InstanceID string `json:"instanceId"`
	SessionID  string `json:"sessionId"`
	Controller bool   `json:"controller"`
}

func adoptedSessionMetaJSON(agentName, instanceID string, sessionID libacp.SessionID, controller bool) json.RawMessage {
	return mustJSON(map[string]any{
		AgentMetaKey: agentName,
		AdoptMetaKey: adoptResult{InstanceID: instanceID, SessionID: string(sessionID), Controller: controller},
	})
}

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

// resolveAdoptTarget validates that ref names a session it is legitimate to attach
// a viewer to. Every rejection is InvalidParams.
func (t *Transport) resolveAdoptTarget(ctx context.Context, ref adoptRef) (agentinstance.InstanceStatus, error) {
	_ = ctx
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
			fmt.Sprintf("could not look up instance %q to adopt: %v", ref.InstanceID, err))
	}
	if st.State != agentinstance.StateRunning {
		return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.adopt: instance %q is %s, not running; only a running instance can be adopted", ref.InstanceID, st.State)
	}

	// SessionIDs is the kernel's set of open sessions, not the viewer hub, so a
	// session that has emitted nothing yet is still adoptable.
	if !containsSessionID(st.SessionIDs, ref.SessionID) {
		return agentinstance.InstanceStatus{}, libacp.NewErrorf(libacp.ErrInvalidParams,
			"contenox.adopt: session %q is not live on instance %q", ref.SessionID, ref.InstanceID)
	}
	return st, nil
}

func containsSessionID(ids []string, sid string) bool {
	for _, id := range ids {
		if id == sid {
			return true
		}
	}
	return false
}

// newAdoptedSession mints an upstream session bound to an already-running
// instance and session and attaches a fresh viewer. Nothing is spawned, and a
// failure here must never stop the instance.
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

	// Attribution comes from the kernel, never the client.
	agentName := st.AgentName

	ag := agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: workspaceID,
		Identity:    "acp-client",
	})
	contenoxSessionID, sessErr := ag.SessionNew(ctx, internalID)
	if sessErr != nil {
		return libacp.NewSessionResponse{}, fmt.Errorf("could not start a session: %w", sessErr)
	}

	bridge := newExternalBridge(t, sessionID, false)
	bridge.setDownstreamID(downstreamID)
	bridge.bindInstance(t.deps.Instances, ref.InstanceID)
	bridge.persistConfigOptions(ctx, bridge.configOptionsSurface())

	// Hold, don't suppress: an adopted session's in-memory journal is its only
	// history, and Attach replays it before this response reaches the client.
	bridge.holdRelay()

	granted, attachErr := t.deps.Instances.Attach(ctx, ref.InstanceID, downstreamID, bridge)
	if attachErr != nil {
		bridge.detachFrom(t)
		return libacp.NewSessionResponse{}, libacp.InternalError(
			fmt.Sprintf("could not adopt session %q on instance %q: %v", ref.SessionID, ref.InstanceID, attachErr))
	}

	entry := &sessionEntry{
		WorkspaceID:       workspaceID,
		Cwd:               sessionCwd,
		InternalSessionID: contenoxSessionID,
		HITLPolicy:        hitlPolicyDefaultValue,
		driver: &externalDriver{
			t:            t,
			agentName:    agentName,
			upstreamID:   sessionID,
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
	// Adoption is a one-time binding: persist the ids so a later session/load
	// re-attaches through the ordinary path.
	t.persistSessionAgent(ctx, store, sessionID, agentName)
	t.persistSessionInstance(ctx, sessionID, ref.InstanceID)
	t.persistSessionDownstream(ctx, sessionID, downstreamID)
	t.clearToolCallState(sessionID)

	libacp.AfterResponse(ctx, func() {
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
