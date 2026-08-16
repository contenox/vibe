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
// contenox session id.
var ErrSessionNotLive = errors.New("acpsvc: no live session for that contenox id on this connection")

// MissionDispatcher is the slice of fleetservice.Service that /mission needs.
type MissionDispatcher interface {
	Dispatch(ctx context.Context, req fleetservice.DispatchRequest) (fleetservice.DispatchResult, error)
}

// MissionAgentResolver resolves the agent name in /mission's arguments.
type MissionAgentResolver interface {
	GetByName(ctx context.Context, name string) (*runtimetypes.Agent, error)
}

// MissionEnvelope is one HITL policy file a mission can be fired under.
type MissionEnvelope struct {
	Name    string
	Path    string
	Summary string
}

// MissionEnvelopeSource lists the envelopes /mission offers and resolves the
// one it is asked to fire under.
type MissionEnvelopeSource interface {
	ListEnvelopes() []MissionEnvelope
	LookupEnvelope(name string) (MissionEnvelope, bool)
}

func (t *Transport) hasMissionCapability() bool {
	return t.deps.Fleet != nil && t.deps.Agents != nil
}

func (t *Transport) handleMission(ctx context.Context, sess *sessionEntry, args string) (string, error) {
	if !t.hasMissionCapability() {
		return "", fmt.Errorf("mission dispatch is unavailable in this session: /mission needs a configured model and the in-process fleet. Configure a model with `contenox config set default-model …` and fire /mission from your editor session.")
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if strings.TrimSpace(args) == "" {
		return t.missionStatus(ctx, store), nil
	}

	flags, rest, err := parseMissionFlags(args)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rest) == "" {
		return "", fmt.Errorf("%s — /mission with no arguments lists the envelopes", missionUsageLine)
	}

	agentName, intent, named := t.resolveMissionAgentAndIntent(ctx, store, rest)
	if strings.TrimSpace(agentName) == "" {
		return "", fmt.Errorf("no mission agent: name one as `/mission <agent-name> <intent>`, or set a default with `contenox config set default-mission-agent <name>`")
	}
	envelope, origin, err := t.resolveMissionEnvelope(ctx, store, flags.policy)
	if err != nil {
		return "", err
	}

	res, err := t.deps.Fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:       agentName,
		Intent:          intent,
		HITLPolicyName:  envelope.Name,
		ParentSessionID: sess.InternalSessionID,
	})
	if err != nil {
		return "", err
	}

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
	var b strings.Builder
	fmt.Fprintf(&b, "Mission fired at %s %q under envelope %q (%s).\n", agentRole, agentName, envelope.Name, origin)
	if envelope.Summary != "" {
		fmt.Fprintf(&b, "Envelope: %s\n", envelope.Summary)
	}
	fmt.Fprintf(&b, "Intent: %s\n", intent)
	fmt.Fprintf(&b, "Mission %s (instance %s, session %s). %s",
		res.MissionID, res.InstanceID, res.SessionID,
		"Reports arrive live in this session as the mission runs; if this session has ended when one lands, it waits in the operator inbox.")
	return b.String(), nil
}

const missionUsageLine = "usage: /mission [--policy <envelope>] [agent-name] <intent>"

const missionPolicyFlag = "policy"

// missionFlags are the leading options /mission accepts; parsing stops at the
// first non-flag token.
type missionFlags struct {
	policy string
}

func parseMissionFlags(args string) (missionFlags, string, error) {
	var f missionFlags
	rest := strings.TrimSpace(args)
	for {
		token, after := splitFirstToken(rest)
		if !strings.HasPrefix(token, "--") {
			return f, rest, nil
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(token, "--"), "=")
		switch name {
		case missionPolicyFlag:
			if !hasValue {
				value, after = splitFirstToken(after)
			}
			value = strings.TrimSpace(value)
			if value == "" || strings.HasPrefix(value, "--") {
				return f, "", fmt.Errorf("--policy needs an envelope name: %s — /mission with no arguments lists the envelopes", missionUsageLine)
			}
			f.policy = value
			rest = after
		default:
			return f, "", fmt.Errorf("unknown /mission flag %q: the only flag is --policy <envelope>, and flags must come before the agent and intent (%s)", token, missionUsageLine)
		}
	}
}

func (t *Transport) resolveMissionEnvelope(ctx context.Context, store runtimetypes.Store, flag string) (MissionEnvelope, string, error) {
	name, origin := strings.TrimSpace(flag), "--policy"
	if name == "" {
		name, origin = strings.TrimSpace(clikv.Read(ctx, store, missionPolicyConfigKey)), "default-mission-policy"
	}
	if name == "" {
		return MissionEnvelope{}, "", fmt.Errorf("no mission envelope: name one as `/mission --policy <envelope> <intent>`, or set a default with `contenox config set %s <envelope>` — a mission must name the HITL policy that bounds it. /mission with no arguments lists the envelopes", missionPolicyConfigKey)
	}
	src := t.deps.MissionEnvelopes
	if src == nil {
		return MissionEnvelope{Name: name}, origin, nil
	}
	env, ok := src.LookupEnvelope(name)
	if !ok {
		return MissionEnvelope{}, "", fmt.Errorf("unknown mission envelope %q (%s): no such policy file on the search path%s", name, origin, missionEnvelopeHint(src.ListEnvelopes()))
	}
	return env, origin, nil
}

const missionPolicyConfigKey = "default-mission-policy"

func missionEnvelopeHint(envelopes []MissionEnvelope) string {
	if len(envelopes) == 0 {
		return " and none were found (run `contenox init` to seed the presets)"
	}
	names := make([]string, 0, len(envelopes))
	for _, e := range envelopes {
		names = append(names, e.Name)
	}
	return ". Available: " + strings.Join(names, ", ")
}

func (t *Transport) missionStatus(ctx context.Context, store runtimetypes.Store) string {
	var b strings.Builder
	b.WriteString("Fire a mission from this session:\n")
	b.WriteString("  /mission <intent>                              the default agent, the default envelope\n")
	b.WriteString("  /mission <agent-name> <intent>                 a named declared agent\n")
	b.WriteString("  /mission --policy <envelope> [agent] <intent>  bound this one mission differently\n")

	agent := strings.TrimSpace(clikv.Read(ctx, store, "default-mission-agent"))
	if agent == "" {
		agent = "(none — set `contenox config set default-mission-agent <name>`)"
	}
	defaultEnvelope := strings.TrimSpace(clikv.Read(ctx, store, missionPolicyConfigKey))
	if defaultEnvelope == "" {
		defaultEnvelope = "(none — set `contenox config set " + missionPolicyConfigKey + " <envelope>`)"
	}
	fmt.Fprintf(&b, "\nDefault agent:    %s\nDefault envelope: %s\n", agent, defaultEnvelope)

	if t.deps.MissionEnvelopes != nil {
		envelopes := t.deps.MissionEnvelopes.ListEnvelopes()
		if len(envelopes) == 0 {
			b.WriteString("\nNo envelopes found on the policy search path — run `contenox init` to seed the presets.\n")
		} else {
			b.WriteString("\nEnvelopes (--policy <name>):\n")
			for _, e := range envelopes {
				marker := "  "
				if e.Name == defaultEnvelope {
					marker = "* "
				}
				fmt.Fprintf(&b, "%s%s", marker, e.Name)
				if e.Summary != "" {
					fmt.Fprintf(&b, "  %s", e.Summary)
				}
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// resolveMissionAgentAndIntent disambiguates `/mission <intent>` from
// `/mission <agent-name> <intent>`.
func (t *Transport) resolveMissionAgentAndIntent(ctx context.Context, store runtimetypes.Store, args string) (agentName, intent string, named bool) {
	first, rest := splitFirstToken(args)
	if rest != "" && t.deps.Agents != nil {
		if a, err := t.deps.Agents.GetByName(ctx, first); err == nil && a != nil {
			return a.Name, rest, true
		}
	}
	return strings.TrimSpace(clikv.Read(ctx, store, "default-mission-agent")), args, false
}

// DeliverToContenoxSession injects a mission report into a live native session
// on this connection, addressed by the firing session's internal id. It returns
// ErrSessionNotLive when no session on this connection maps to contenoxSessionID.
func (t *Transport) DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error {
	sid, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	t.persistDeliveredReport(ctx, contenoxSessionID, n)
	n.SessionID = sid
	t.sendUpdate(ctx, n)
	return nil
}

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

func splitFirstToken(args string) (first, rest string) {
	args = strings.TrimSpace(args)
	if i := strings.IndexFunc(args, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }); i >= 0 {
		return args[:i], strings.TrimSpace(args[i+1:])
	}
	return args, ""
}

// acpSessionFiredKVPrefix marks a session that has fired a mission, keyed by the
// upstream ACP session id.
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
// keyed by the upstream ACP session id.
const acpSessionMissionKVPrefix = "acp:session_mission:"

type sessionMissionRecord struct {
	MissionID        string   `json:"missionId"`
	ModelAllowlist   []string `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string `json:"backendAllowlist,omitempty"`
	HITLPolicyName   string   `json:"hitlPolicyName,omitempty"`
}

func (t *Transport) persistSessionMission(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, meta missionservice.MissionMeta) {
	if strings.TrimSpace(meta.MissionID) == "" {
		return
	}
	raw, err := json.Marshal(sessionMissionRecord{
		MissionID:        meta.MissionID,
		ModelAllowlist:   meta.ModelAllowlist,
		BackendAllowlist: meta.BackendAllowlist,
		HITLPolicyName:   meta.HITLPolicyName,
	})
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
	return t.readSessionMissionRecord(ctx, store, sid).MissionID
}

func (t *Transport) readSessionMissionRecord(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) sessionMissionRecord {
	var rec sessionMissionRecord
	if err := store.GetKV(ctx, acpSessionMissionKVPrefix+string(sid), &rec); err != nil {
		return sessionMissionRecord{}
	}
	return rec
}

// restoreSessionMission re-attaches a unit session's mission identity after a
// load or resume.
func (t *Transport) restoreSessionMission(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, entry *sessionEntry) {
	rec := t.readSessionMissionRecord(ctx, store, sid)
	if rec.MissionID == "" {
		return
	}
	entry.MissionID = rec.MissionID
	entry.ModelAllowlist = rec.ModelAllowlist
	entry.BackendAllowlist = rec.BackendAllowlist
	entry.HITLPolicy = missionHITLPolicy(rec.HITLPolicyName)
}

func missionHITLPolicy(name string) string {
	if strings.TrimSpace(name) == "" {
		return hitlPolicyDefaultValue
	}
	return strings.TrimSpace(name)
}

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
