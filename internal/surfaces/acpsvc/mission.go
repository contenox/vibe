package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/chatservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

// ErrSessionNotLive reports that no session on this connection maps to a given
// contenox (internal) session id — the firing session has ended, or it was never
// hosted by this transport. It is the signal a mission-report deliverer turns
// into an inbox fallback (mirroring agentinstance.ErrNotFound for the kernel), so
// it is a branchable sentinel.
var ErrSessionNotLive = errors.New("acpsvc: no live session for that contenox id on this connection")

// MissionDispatcher is the narrow slice of fleetservice.Service /mission
// needs: fire and get back ids. serve wires the real Service; stdio
// `contenox acp` leaves it nil, and /mission reports dispatch unavailable
// rather than half-firing.
type MissionDispatcher interface {
	Dispatch(ctx context.Context, req fleetservice.DispatchRequest) (fleetservice.DispatchResult, error)
}

// MissionAgentResolver is the narrow slice of agentregistryservice.Service
// used to disambiguate /mission's two grammar forms (is the first token an
// agent name, or the first word of the intent?).
type MissionAgentResolver interface {
	GetByName(ctx context.Context, name string) (*runtimetypes.Agent, error)
}

// hasMissionCapability reports whether /mission can run: a dispatcher
// (Deps.Fleet) and an agent resolver (Deps.Agents), both wired only by an
// editor embedding the fleet in-process. Gates whether /mission is
// advertised at all (commands.go) — never advertise what cannot work.
func (t *Transport) hasMissionCapability() bool {
	return t.deps.Fleet != nil && t.deps.Agents != nil
}

// handleMission fires a mission from this chat session (`/mission`), setting
// ParentSessionID to this session so its reports are delivered live into this
// session's stream and persisted into its transcript (see
// DeliverToContenoxSession) — falling back to the operator inbox only when no
// live connection holds the session when a report lands. Dispatches through
// fleetservice.Dispatch, the same path the REST API and CLI use.
func (t *Transport) handleMission(ctx context.Context, sess *sessionEntry, args string) (string, error) {
	if !t.hasMissionCapability() {
		return "", fmt.Errorf("mission dispatch is unavailable in this session: /mission needs a configured model and the in-process fleet. Configure a model with `contenox config set default-model …` and fire /mission from your editor session.")
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return "", fmt.Errorf("usage: /mission <intent>   or   /mission <agent-name> <intent>")
	}

	store := runtimetypes.New(t.deps.DB.WithoutTransaction())

	agentName, intent, named := t.resolveMissionAgentAndIntent(ctx, store, args)
	if strings.TrimSpace(agentName) == "" {
		return "", fmt.Errorf("no mission agent: name one as `/mission <agent-name> <intent>`, or set a default with `contenox config set default-mission-agent <name>`")
	}
	policy := strings.TrimSpace(clikv.Read(ctx, store, "default-mission-policy"))
	if policy == "" {
		return "", fmt.Errorf("no mission envelope: set one with `contenox config set default-mission-policy <policy>` — a mission must name the HITL policy that bounds it")
	}

	res, err := t.deps.Fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:      agentName,
		Intent:         intent,
		HITLPolicyName: policy,
		// Empty only if the session carries no internal id, which routes
		// reports to the operator inbox instead.
		ParentSessionID: sess.InternalSessionID,
	})
	if err != nil {
		return "", err
	}

	// The confirmation states plainly which agent was chosen (default vs
	// named), since the two grammar forms are shape-indistinguishable.
	// Firing unlocks this session's supervisor tools, in memory and durably.
	sess.mu.Lock()
	sess.FiredMissions = true
	sid, hasSID := t.acpSessionForContenoxID(sess.InternalSessionID)
	sess.mu.Unlock()
	if hasSID {
		t.persistSessionFiredMission(ctx, store, sid)
	}

	agentRole := "default mission agent"
	if named {
		agentRole = "named agent"
	}
	tail := "Reports arrive live in this session as the mission runs; if this session has ended when one lands, it waits in the operator inbox."
	return fmt.Sprintf(
		"Mission fired at %s %q under envelope %q.\nIntent: %s\nMission %s (instance %s, session %s). %s",
		agentRole, agentName, policy, intent, res.MissionID, res.InstanceID, res.SessionID, tail,
	), nil
}

// resolveMissionAgentAndIntent disambiguates `/mission <intent>` from
// `/mission <agent-name> <intent>`: the first token is resolved against the
// declared-agent registry; a hit is the named form, a miss means the whole
// line is the intent for the configured default agent. named reports which
// branch was taken.
func (t *Transport) resolveMissionAgentAndIntent(ctx context.Context, store runtimetypes.Store, args string) (agentName, intent string, named bool) {
	first, rest := splitFirstToken(args)
	if rest != "" && t.deps.Agents != nil {
		if a, err := t.deps.Agents.GetByName(ctx, first); err == nil && a != nil {
			return a.Name, rest, true
		}
	}
	return strings.TrimSpace(clikv.Read(ctx, store, "default-mission-agent")), args, false
}

// DeliverToContenoxSession injects a mission report into a live native
// session on this connection, addressed by the firing session's internal id
// (the mission's ParentSessionID). It re-addresses the notification to the
// ACP session id the client knows, pushes it as an ordinary session/update
// (carrying reportrouter's `contenox.missionReport` _meta), and persists it
// into the transcript so it survives a reload and enters the next turn's
// history — a best-effort write; the durable fact is the report itself.
//
// Returns ErrSessionNotLive when no session on this connection maps to
// contenoxSessionID, the signal that routes the report to the operator inbox
// instead — never a fault.
func (t *Transport) DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error {
	sid, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	t.persistDeliveredReport(ctx, contenoxSessionID, n)
	// Re-address to the ACP session id; the router built n against the
	// contenox id, which the client never saw.
	n.SessionID = sid
	t.sendUpdate(ctx, n)
	return nil
}

// persistDeliveredReport appends a delivered mission report to the firing
// session's durable transcript as an assistant message. Cancellation-immune
// (context.WithoutCancel): the report already arrived, so the request's
// liveness must not decide whether it is remembered.
func (t *Transport) persistDeliveredReport(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) {
	if t.deps.DB == nil || contenoxSessionID == "" {
		return
	}
	text := strings.TrimSpace(n.Update.Content.Text)
	if text == "" {
		return
	}
	msgs := []taskengine.Message{{
		ID:        uuid.NewString(),
		Role:      "assistant",
		Content:   text,
		Timestamp: time.Now().UTC(),
	}}
	cleanCtx := context.WithoutCancel(ctx)
	mgr := chatservice.NewManager(t.workspaceID())
	if err := mgr.PersistDiff(cleanCtx, t.deps.DB.WithoutTransaction(), contenoxSessionID, msgs); err != nil {
		reportErr, _, end := t.tracker().Start(cleanCtx, "persist", "acp_mission_report", "session_id", contenoxSessionID)
		reportErr(err)
		end()
	}
}

// splitFirstToken splits args into its first whitespace-delimited token and the
// trimmed remainder. A single-token input yields an empty remainder.
func splitFirstToken(args string) (first, rest string) {
	args = strings.TrimSpace(args)
	if i := strings.IndexFunc(args, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }); i >= 0 {
		return args[:i], strings.TrimSpace(args[i+1:])
	}
	return args, ""
}

// acpSessionFiredKVPrefix marks a session that has fired a mission, keyed by
// the upstream ACP session id. Unlocks the supervisor tools (mission_list /
// mission_answer) for that session only; durable so they survive a reload.
const acpSessionFiredKVPrefix = "acp:session_fired_missions:"

type sessionFiredRecord struct {
	Fired bool `json:"fired"`
}

func (t *Transport) persistSessionFiredMission(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) {
	raw, err := json.Marshal(sessionFiredRecord{Fired: true})
	if err != nil {
		return
	}
	if err := store.SetKV(ctx, acpSessionFiredKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_fired_mission", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionFiredMission(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) bool {
	var rec sessionFiredRecord
	if err := store.GetKV(ctx, acpSessionFiredKVPrefix+string(sid), &rec); err != nil {
		return false
	}
	return rec.Fired
}

// acpSessionMissionKVPrefix stores the mission id a session is the unit of,
// keyed by the upstream ACP session id, written at session/new for a fleet
// dispatch's session. Durable half of an attribution session/list needs on a
// fresh connection, where the in-memory sessionEntry doesn't exist — without
// it a dispatched unit's session is indistinguishable from an ordinary chat.
const acpSessionMissionKVPrefix = "acp:session_mission:"

// sessionMissionRecord is the durable KV shape for a unit session's mission id.
type sessionMissionRecord struct {
	MissionID string `json:"missionId"`
}

func (t *Transport) persistSessionMission(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, missionID string) {
	if strings.TrimSpace(missionID) == "" {
		return
	}
	raw, err := json.Marshal(sessionMissionRecord{MissionID: missionID})
	if err != nil {
		return
	}
	if err := store.SetKV(ctx, acpSessionMissionKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_mission", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

func (t *Transport) readSessionMission(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	var rec sessionMissionRecord
	if err := store.GetKV(ctx, acpSessionMissionKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.MissionID
}

// sessionListMeta builds a session/list entry's `_meta` carrying whichever
// attributions the session has (external agent name, mission id, or both).
// Returns nil when it has neither.
func sessionListMeta(agentName, missionID string) json.RawMessage {
	meta := map[string]any{}
	if agentName != "" {
		meta[AgentMetaKey] = agentName
	}
	if missionID != "" {
		meta[missionservice.MissionMetaKey] = missionservice.MissionMeta{MissionID: missionID}
	}
	if len(meta) == 0 {
		return nil
	}
	return mustJSON(meta)
}
