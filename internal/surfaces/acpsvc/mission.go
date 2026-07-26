package acpsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

// ErrSessionNotLive reports that no session on this connection maps to a given
// contenox (internal) session id — the firing session has ended, or it was never
// hosted by this transport. It is the signal a mission-report deliverer turns
// into an inbox fallback (mirroring agentinstance.ErrNotFound for the kernel), so
// it is a branchable sentinel.
var ErrSessionNotLive = errors.New("acpsvc: no live session for that contenox id on this connection")

// MissionDispatcher is the narrow slice of fleetservice.Service the /mission
// slash command needs: fire a mission and get back its ids. Kept as a local
// interface (not the whole Service) so this package depends only on the one
// method it uses and a fake is trivial in tests — the house rule that a command
// accepts the narrowest interface it actually needs. serve satisfies it with the
// real fleetservice.Service; the stdio `contenox acp` path leaves it nil, and
// /mission then reports that dispatch is unavailable rather than half-firing.
type MissionDispatcher interface {
	Dispatch(ctx context.Context, req fleetservice.DispatchRequest) (fleetservice.DispatchResult, error)
}

// MissionAgentResolver is the narrow slice of agentregistryservice.Service used
// to disambiguate /mission's two shape-identical grammar forms — is the first
// token an agent name, or the first word of the intent? A hit means the named
// form. Only GetByName is needed here, so that is all this interface asks for.
type MissionAgentResolver interface {
	GetByName(ctx context.Context, name string) (*runtimetypes.Agent, error)
}

// hasMissionCapability reports whether this transport can run /mission at all: a
// dispatcher to fire through (Deps.Fleet) AND an agent resolver to parse the
// two-shape grammar (Deps.Agents). Both are wired by a `contenox acp` editor
// that embeds the fleet IN-PROCESS (a mission is a subagent of THIS process;
// see runtime/contenoxcli/acp_cmd.go).
//
// This single bit gates whether /mission is ADVERTISED (acpCommands, commands.go
// — never advertise what cannot work): an editor that embeds the fleet always
// advertises /mission. A process that is ITSELF a dispatched unit, or a
// setup-only editor with no model, leaves both nil and never lists it.
func (t *Transport) hasMissionCapability() bool {
	return t.deps.Fleet != nil && t.deps.Agents != nil
}

// handleMission fires a mission FROM this chat session — the `/mission` slash
// command (fleet-consolidation.md M4, "the surface an operator actually reaches
// for"). It sets the mission's ParentSessionID to this session, which is what
// makes the supervision edge real from the chat side: the fired unit's reports
// belong to whoever is driving THIS session.
//
// It dispatches through fleetservice.Dispatch — the same orchestration the REST
// path and `contenox mission fire` use — rather than reimplementing anything, so
// the Enabled gate, the envelope, teardown-on-failure, and the mission record
// are all the shared implementation.
//
// # Where reports go: this session, live
//
// The governing ontology: a mission is a
// SUBAGENT of the process that fired it, and its report notifies exactly the
// parent that fired it. The editor embeds the fleet and its report router
// (acp_cmd.go) and reaches its lone transport directly, so the fired unit's
// report is DELIVERED live into THIS session's stream and PERSISTED into its
// transcript — still there after a reload, entering the next turn's history;
// see DeliverToContenoxSession below. The operator inbox is only the fallback
// for when no live connection holds this session by the time a report lands
// (never an error — a report is a durable fact).
func (t *Transport) handleMission(ctx context.Context, sess *sessionEntry, args string) (string, error) {
	if !t.hasMissionCapability() {
		// No fleet is wired in-process: a setup-only editor with no model yet, or a
		// process that is ITSELF a dispatched unit (a subagent does not host its own
		// fleet). /mission is not advertised here, so reaching this is a stale menu or
		// a remembered command. Teach the in-process path.
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
		// The supervision edge: this session FIRED the mission, so its upstream
		// contenox session id is the parent. Empty only if the session somehow
		// carries no internal id, which routes reports to the operator inbox — the
		// same fallback as an operator firing directly.
		ParentSessionID: sess.InternalSessionID,
	})
	if err != nil {
		return "", err
	}

	// The two grammar forms are indistinguishable by shape, so the confirmation
	// states PLAINLY which agent was chosen (default vs named) and echoes the
	// intent verbatim — the blueprint's chosen mitigation for the ambiguity,
	// making a misread visible in the transcript the instant it happens rather
	// than letting a first intent word that happens to match an agent name change
	// meaning silently.
	// This session now supervises something, which is what unlocks its supervisor
	// tools — in memory for the turns that follow on this connection, and durably
	// so a reload keeps them.
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
	// The report-routing tail is honest about where reports actually go. The
	// in-process fleet supervises the mission from THIS session, so its reports
	// arrive here live; the operator inbox is only the fallback for a session
	// that has ended when a report lands.
	tail := "Reports arrive live in this session as the mission runs; if this session has ended when one lands, it waits in the operator inbox."
	return fmt.Sprintf(
		"Mission fired at %s %q under envelope %q.\nIntent: %s\nMission %s (instance %s, session %s). %s",
		agentRole, agentName, policy, intent, res.MissionID, res.InstanceID, res.SessionID, tail,
	), nil
}

// resolveMissionAgentAndIntent decides the mission's agent and intent from the
// command's arguments, resolving the grammar ambiguity fleet-consolidation.md M4
// flags: `/mission <intent>` and `/mission <agent-name> <intent>` are the same
// shape. The rule (deliberate, per the blueprint): resolve the FIRST token
// against the declared-agent registry — a hit is the named form (agent = token,
// intent = the rest); a miss means the whole line is the intent for the
// configured default agent. named reports which branch was taken so the caller's
// confirmation can name it. With no resolver wired, or a single-token input,
// only the default form is possible.
func (t *Transport) resolveMissionAgentAndIntent(ctx context.Context, store runtimetypes.Store, args string) (agentName, intent string, named bool) {
	first, rest := splitFirstToken(args)
	if rest != "" && t.deps.Agents != nil {
		if a, err := t.deps.Agents.GetByName(ctx, first); err == nil && a != nil {
			return a.Name, rest, true
		}
	}
	return strings.TrimSpace(clikv.Read(ctx, store, "default-mission-agent")), args, false
}

// DeliverToContenoxSession injects an out-of-band update — a mission report the
// report router routed on the supervision edge — into a LIVE native session on
// THIS stdio connection, addressed by the firing session's contenox (internal)
// session id (the mission's ParentSessionID, which handleMission set above).
//
// It is the in-process editor's half of the supervision edge the ontology
// demands: a mission is a subagent of the
// process that fired it, and its report notifies exactly the parent session that
// fired it — which, for a `/mission` fired from the editor, is one of this
// transport's OWN native sessions, not a kernel-owned unit. The report router's
// SessionDeliverer reaches it here (see runtime/contenoxcli/acp_cmd.go): the
// firing session id is mapped to the ACP session id the client knows
// (contenoxToACPID), the notification is re-addressed to it (exactly as the
// kernel's DeliverToSession stamps the owning id), and pushed to the editor as an
// ordinary session/update — carrying the reportrouter's `contenox.missionReport`
// _meta so the editor renders it as a report, not chat text.
//
// The delivered report is also PERSISTED into the session's transcript, not only
// pushed. A pushed update lives on the wire: reload the editor and the report is
// gone, even though the mission is done and the operator was watching — the very
// gap the supervision edge exists to close. Persisting also makes the report real
// to the SESSION rather than to the connection: the next turn's history carries
// it, which is what lets a coordinating agent act on "unit X reported: …" the way
// the report router's own doc describes. It is best-effort by the same rule the
// router follows — the durable fact is the report itself, so a failed write is
// tracked and never turns a successful delivery into a miss.
//
// Returns ErrSessionNotLive when no session on this connection maps to
// contenoxSessionID (it has ended, or was never here), the signal that routes the
// report to the operator inbox instead — never a fault.
func (t *Transport) DeliverToContenoxSession(ctx context.Context, contenoxSessionID string, n libacp.SessionNotification) error {
	sid, ok := t.acpSessionForContenoxID(contenoxSessionID)
	if !ok {
		return ErrSessionNotLive
	}
	t.persistDeliveredReport(ctx, contenoxSessionID, n)
	// Re-address to the ACP session id the client learned at session/new; the
	// router built n against the contenox id, which the editor never saw.
	n.SessionID = sid
	t.sendUpdate(ctx, n)
	return nil
}

// persistDeliveredReport appends a delivered mission report to the firing
// session's durable transcript as an assistant message, so it survives a reload
// and enters the next turn's history. The text is the one the report router
// already composed for the stream, so the transcript and the live update read
// identically.
//
// Cancellation-immune (context.WithoutCancel) for the same reason
// persistExternalTurn is: the report arrived: whether the request that carried it
// is still alive must not decide whether it is remembered.
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

// acpSessionFiredKVPrefix marks a session as one that has FIRED missions —
// written the first time `/mission` succeeds there, keyed by the upstream ACP
// session id.
//
// It is what unlocks the supervisor tools (mission_list / mission_answer) for
// that session, and why they are absent everywhere else: an ordinary chat has
// nothing to supervise, so offering it two mission tools would be surface it can
// only misuse. Durable rather than in-memory because the tools must still be
// there after a reload — the missions certainly are.
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

// acpSessionMissionKVPrefix stores the mission id a session is the UNIT of,
// keyed by the upstream ACP session id and written at session/new for a session
// a fleet dispatch created (the mission id arrives as session/new `_meta`; see
// NewSession). It is the durable half of an attribution the in-memory
// sessionEntry already carried: session/list runs on a fresh connection, where
// that entry does not exist.
//
// It is what lets a client TELL THE TWO APART. A dispatched unit's session is
// created in the same workspace under the same 'acp-client' identity as the
// operator's own chats, so without this every mission unit showed up in beam's
// sidebar as an anonymous chat — and, having real messages while the session
// that fired it had none, sorted ABOVE it.
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

// sessionListMeta builds a session/list entry's `_meta`, carrying whichever
// attributions the session has: its external agent name, its mission id, or
// both (an external agent CAN be the unit of a mission). Returns nil when it has
// neither, so an ordinary chat session's entry stays free of an empty envelope.
//
// One builder rather than two assignments: the two attributions are independent
// and a second `info.Meta = …` would silently drop the first.
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
