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

// MissionEnvelope is one HITL policy file a mission can be fired under, as
// the policy loader would resolve the name.
type MissionEnvelope struct {
	// Name is the file name (`hitl-policy-strict.json`) --policy takes and
	// the mission record stores.
	Name string
	// Path is the file the loader reads for Name; "" when unknown.
	Path string
	// Summary is a one-line character sketch read from the file itself
	// (default action, tool-call ceiling, agent-answer posture), or "".
	Summary string
}

// MissionEnvelopeSource lists the envelopes /mission offers and resolves the
// one it is asked to fire under. The host surface implements it because the
// host owns the policy search path; nil leaves /mission with no listing and
// no pre-dispatch check, so an unknown name reaches the unit's own loader
// instead of being refused here.
type MissionEnvelopeSource interface {
	// ListEnvelopes returns the envelopes on the policy search path, one entry
	// per name, resolved first-directory-wins like the loader.
	ListEnvelopes() []MissionEnvelope
	// LookupEnvelope resolves one name; ok is false when no directory on the
	// search path holds it.
	LookupEnvelope(name string) (MissionEnvelope, bool)
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
// fleetservice.Dispatch, the same path the REST API and CLI use. With no
// arguments it fires nothing and prints the envelope listing instead, the
// same "show, then set" shape /policy and /model have.
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
		AgentName:      agentName,
		Intent:         intent,
		HITLPolicyName: envelope.Name,
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
	// The bounds the operator just accepted, stated where they accepted them:
	// the CLI prints agent + envelope on every fire, and a session must not be
	// quieter about what it just let loose.
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

// missionUsageLine is the one-line grammar, repeated verbatim by every
// /mission refusal so the shape is learned from whichever error comes first.
const missionUsageLine = "usage: /mission [--policy <envelope>] [agent-name] <intent>"

// missionPolicyFlag names the envelope one fire runs under, overriding the
// default-mission-policy config for that mission only — the session-level
// /policy is a different setting and does not bound a dispatched unit.
const missionPolicyFlag = "policy"

// missionFlags are the leading options /mission accepts. Flags must lead:
// parsing stops at the first non-flag token, so a "--" inside an intent stays
// literal text rather than becoming a lever.
type missionFlags struct {
	policy string
}

// parseMissionFlags peels the leading flags off /mission's arguments and
// returns the untouched remainder (the agent and intent).
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

// resolveMissionEnvelope picks the envelope one fire runs under: the --policy
// flag, else the default-mission-policy config. The name is checked against
// the policy search path when a source is wired, so a typo is refused here —
// with the list — instead of reaching a unit whose loader would fall through
// to a default nobody chose. origin names where the envelope came from, for
// the confirmation.
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

// missionPolicyConfigKey is the config key holding the envelope /mission
// falls back to, shared with `contenox mission fire`'s own default.
const missionPolicyConfigKey = "default-mission-policy"

// missionEnvelopeHint lists the envelopes an unknown name could have meant,
// so a typo is answered with the answer rather than a second command.
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

// missionStatus is what `/mission` alone answers: the grammar, the defaults
// in force, and the envelopes on the policy search path with each one's
// character — the discovery surface, so nobody has to remember a filename.
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

// sessionMissionRecord is the durable KV shape for a unit session's mission
// identity: the id its mission tools are scoped to, and the compute allowlists
// its envelope resolved. The allowlists are stored alongside the id because a
// session restored without them would resolve models the envelope excluded.
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
// load or resume. Without it a reloaded unit is indistinguishable from an
// ordinary chat: GetToolsForToolsByName lists no mission tools, so the unit
// cannot report, plan or finish, and the run only ends when the drive loop gives
// up on it. The mirror of readSessionFiredMission, which restores the other half
// of the relationship — that this session HAS units rather than IS one.
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

// missionHITLPolicy resolves the envelope a unit's own tool calls are gated by.
// A mission that named none falls back to the host's policy, which is the
// pre-mission behaviour; a mission that named one must be bound by it.
func missionHITLPolicy(name string) string {
	if strings.TrimSpace(name) == "" {
		return hitlPolicyDefaultValue
	}
	return strings.TrimSpace(name)
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
