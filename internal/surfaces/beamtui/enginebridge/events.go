package enginebridge

import (
	"encoding/json"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/surfaces/acpsvc"
	libacp "github.com/contenox/libacp"
)

// Event is one fact produced by the runtime, already destructured out of the
// ACP wire types. The set is closed: only the types in this file implement it.
//
// Exactly one Event is produced per inbound notification the active-session
// filter admits (SetActiveSession discards updates for any other session
// before translation); no coalescing, no reordering, and a kind this package
// does not model becomes UnknownUpdate rather than nothing. Out-of-band
// events (permission, turn and shell results) don't ride the notification
// stream and aren't filtered.
//
// Every Event carries the session it belongs to (SessionOf) so a consumer can
// route without a side table; with an active session set, only that
// session's updates are admitted, so several live transcripts at once need
// either an unfiltered window or one Bridge per session.
type Event interface {
	// SessionOf reports the ACP session this fact belongs to.
	SessionOf() libacp.SessionID
	isBridgeEvent()
}

// TextDelta is one streamed chunk of assistant prose (agent_message_chunk).
// MessageID groups chunks sharing one message; empty means the agent did not
// group this (the field is optional in the spec).
type TextDelta struct {
	SessionID libacp.SessionID
	MessageID string
	Text      string
}

// ThoughtDelta is one streamed chunk of reasoning (agent_thought_chunk).
// MessageID groups it exactly like TextDelta.
type ThoughtDelta struct {
	SessionID libacp.SessionID
	MessageID string
	Text      string
}

// UserEcho is a user_message_chunk the agent sent back — in practice the
// transcript replay session/load performs, not an echo of what the operator
// just typed (the Bridge never synthesizes one).
type UserEcho struct {
	SessionID libacp.SessionID
	MessageID string
	Text      string
}

// ToolCallOpened is the first notification for a tool call (tool_call). A
// later state change for the same ToolCallID arrives as ToolCallUpdated.
type ToolCallOpened struct {
	SessionID  libacp.SessionID
	ToolCallID string
	Title      string
	Kind       libacp.ToolKind
	Status     libacp.ToolCallStatus
	Contents   []libacp.ToolCallContent
	Locations  []libacp.ToolCallLocation
	RawInput   json.RawMessage
}

// ToolCallUpdated is a state change for an already-opened tool call
// (tool_call_update); fields the agent did not restate arrive zero, and the
// contract is patch-shaped — a renderer merges onto the existing card. An
// empty field always means "unchanged", never "cleared": the wire has no
// erasure sentinel, so nothing can remove content from an open tool call.
type ToolCallUpdated struct {
	SessionID  libacp.SessionID
	ToolCallID string
	Title      string
	Kind       libacp.ToolKind
	Status     libacp.ToolCallStatus
	Contents   []libacp.ToolCallContent
	Locations  []libacp.ToolCallLocation
	RawInput   json.RawMessage
	RawOutput  json.RawMessage
}

// PlanUpdated carries the agent's whole current plan (plan). Entries replace
// the previous list; it is never a delta.
type PlanUpdated struct {
	SessionID libacp.SessionID
	Entries   []libacp.PlanEntry
}

// UsageUpdated is the session context indicator (usage_update) and the only
// channel for token accounting. Size == 0 is a reachable wire shape (a
// session with no configured context budget), not a bug: consumers must
// render Used absolutely ("12,481 tokens"), never as Used/Size, which
// divides by zero.
type UsageUpdated struct {
	SessionID libacp.SessionID
	Used      int
	Size      int
	Cost      *libacp.UsageCost
}

// CommandsUpdated is the slash-command menu the agent advertises
// (available_commands_update). It exists for autocomplete only: an invoked
// command goes back through SubmitPrompt as plain text and acpsvc's
// parseCommand intercepts it server-side.
type CommandsUpdated struct {
	SessionID libacp.SessionID
	Commands  []libacp.AvailableCommand
}

// ConfigOptionUpdated carries the session's config selects
// (config_option_update): model, HITL policy, think level, token limit,
// workspace root; Options replaces the previous list.
//
// It arrives both as the wire notification (after /model, /provider,
// /think, /policy or set_config_option) and as a replay of a session's
// opening options from session/new, session/load or session/resume
// responses (see (*Bridge).emitInitialConfigOptions) — a consumer must not
// care which.
type ConfigOptionUpdated struct {
	SessionID libacp.SessionID
	Options   []libacp.SessionConfigOption
}

// ValueDomains projects config options onto the argument domains of the
// slash commands that take a value (/model, /provider, /think, /policy), so
// a completing surface matches what the server validates (see
// acpsvc.CommandValueDomains). An absent key means "no domain known" — treat
// as "anything the operator types is fine", never as a gate.
func ValueDomains(options []libacp.SessionConfigOption) map[string][]string {
	return acpsvc.CommandValueDomains(options)
}

// ModeUpdated reports the session's current mode id (current_mode_update).
type ModeUpdated struct {
	SessionID libacp.SessionID
	ModeID    string
}

// ReplayEnded marks the end of a LoadSession transcript replay. It is
// bridge-synthesized, emitted after the load RPC's response so it follows
// every replayed notification; consumers settle the trailing replayed
// message on it.
type ReplayEnded struct {
	SessionID libacp.SessionID
}

// SessionInfoUpdated carries session metadata pushed after a turn
// (session_info_update): the derived title and the activity timestamp session
// pickers sort on.
type SessionInfoUpdated struct {
	SessionID libacp.SessionID
	Title     string
	UpdatedAt string
}

// MissionReport is a report from a dispatched mission, delivered into the
// session that fired it. On the wire it's an agent_message_chunk carrying a
// contenox.missionReport _meta envelope; the Bridge emits this instead of
// TextDelta, so one notification stays one event.
type MissionReport struct {
	SessionID libacp.SessionID
	MissionID string
	ReportID  string
	Kind      string
	AgentName string
	MessageID string
	Text      string
}

// MissionAsk is an attention question from a dispatched mission unit, which
// blocks until it is answered (AskID is what an answer is given against).
// Same wire shape as MissionReport (contenox.missionAsk _meta), emitted
// instead of TextDelta. Answering is not yet a Bridge method.
type MissionAsk struct {
	SessionID libacp.SessionID
	MissionID string
	AskID     string
	AgentName string
	Intent    string
	Summary   string
	Detail    string
	MessageID string
	Text      string
}

// MissionStatusChanged is a dispatched mission coming to rest — or opening —
// announced into the session that fired it. Same carrier as MissionReport
// (contenox.missionStatus _meta).
//
// Old and New are missionservice.Status values as strings, not a typed enum,
// since this package models the wire; a consumer switching on New must have
// a default arm for a status this build does not recognize. Reason is the
// one line the finisher gave, empty for a clean landing.
type MissionStatusChanged struct {
	SessionID libacp.SessionID
	MissionID string
	AgentName string
	Old       string
	New       string
	Reason    string
	MessageID string
}

// MissionPlanRevised is a dispatched mission's planner replacing its plan,
// announced into the firing session. Same carrier and rules as
// MissionStatusChanged (contenox.missionPlan _meta).
//
// Revision is the plan's monotonic number, Explanation the planner's
// one-line "why". Pending/InProgress/Completed need not sum to EntryCount —
// the status vocabulary may grow — so a renderer shows only what it knows.
// A plan revision is not an attention event.
type MissionPlanRevised struct {
	SessionID   libacp.SessionID
	MissionID   string
	AgentName   string
	Revision    int
	Explanation string
	EntryCount  int
	Pending     int
	InProgress  int
	Completed   int
	MessageID   string
}

// The mission lifecycle vocabulary MissionStatusChanged.Old and .New carry,
// mirroring missionservice.Status. Duplicated here (rather than imported)
// because this package models the wire; the set is closed by convention, not
// by a Go type — see MissionStatusChanged on why consumers need a default arm.
const (
	MissionStatusOpen      = "open"
	MissionStatusLanded    = "landed"
	MissionStatusDerailed  = "derailed"
	MissionStatusStuck     = "stuck"
	MissionStatusAbandoned = "abandoned"
)

// MissionStatusTerminal reports whether status is one a mission comes to
// rest in — the work is over and nothing further will arrive from that
// unit. "open" is not terminal, and neither is a status this build does not
// recognize, which is treated as still-running.
func MissionStatusTerminal(status string) bool {
	switch status {
	case MissionStatusLanded, MissionStatusDerailed, MissionStatusStuck, MissionStatusAbandoned:
		return true
	}
	return false
}

// InboxItemAdded is a mission report that reached no live supervising
// session and was written to the durable operator inbox instead
// (internal/services/operatorinbox). It is the one event that does not come
// off the ACP wire — it arrives on the process bus, since there was no
// session to deliver it into.
//
// SessionOf always returns the empty SessionID: nobody was watching, so a
// consumer must not route it by session or let a session filter drop it.
//
// Reason is why it landed here ("operator_fired": nobody was ever
// supervising; "parent_gone": the supervisor ended before the report
// arrived).
type InboxItemAdded struct {
	ID        string
	MissionID string
	AgentName string
	Intent    string
	Reason    string
	// Kind and Summary are the embedded report's own: kind ("progress",
	// "finding", "blocker", "result") and its one-line summary.
	Kind    string
	Summary string
}

// TerminalChunk is live output from the session's persistent shell, carried
// by acpsvc's extension update kind. Reset marks the scrollback snapshot
// delivered on (re)subscribe: the consumer must replace its buffer, not
// append. Offset is Chunk's byte offset within that scrollback.
type TerminalChunk struct {
	SessionID libacp.SessionID
	Offset    int64
	Chunk     string
	Reset     bool
}

// UnknownUpdate is a session/update whose kind this package does not model. It
// exists so the no-drops contract survives a protocol addition: the raw update
// is handed through verbatim and the consumer may ignore it.
type UnknownUpdate struct {
	SessionID libacp.SessionID
	Kind      libacp.SessionUpdateKind
	Update    libacp.SessionUpdate
}

// PermissionRequested is a HITL gate: the tool call named here blocks until
// Resolve is called, blocking only that call — the rest of the turn's stream
// and other sessions are unaffected.
//
// Resolve is idempotent and safe from any goroutine; the first call wins.
// allow=true answers "allow", false "deny". A request left unresolved when
// the Bridge closes or the turn is cancelled resolves itself as cancelled.
// See PermissionResolved, which is what a card should retire on.
type PermissionRequested struct {
	SessionID  libacp.SessionID
	ToolCallID string
	Title      string
	Kind       libacp.ToolKind
	Status     libacp.ToolCallStatus
	// Meta is approvalflow's decoded envelope: which tool, under which named
	// policy, and the diff a card renders. Zero when the peer sent no _meta.
	Meta      approvalflow.Meta
	Contents  []libacp.ToolCallContent
	Locations []libacp.ToolCallLocation
	RawInput  json.RawMessage
	// Options is what the agent offered, verbatim (always
	// [allow(allow_once), deny(reject_once)]) so a card can label its keys.
	Options []libacp.PermissionOption
	Resolve func(allow bool)
}

// PermissionResolved is emitted when a pending permission request reaches
// any terminal state (operator answered, turn cancelled, or bridge torn
// down), so a card retires deterministically. Exactly one follows each
// PermissionRequested, matched on ToolCallID.
//
// Outcome is PermissionOutcomeSelected when the operator chose (the choice
// itself surfaces as the tool call's status, not here), or
// PermissionOutcomeCancelled otherwise. It does not arrive on teardown: the
// consumer learns the same fact when Events() closes, retiring every card.
type PermissionResolved struct {
	SessionID  libacp.SessionID
	ToolCallID string
	Outcome    libacp.PermissionOutcomeKind
}

// TurnEnded reports that a submitted prompt finished. A genuine cancel lands
// here with StopReason "cancelled" and is not an error; TurnFailed means the
// turn never produced a stop reason at all.
type TurnEnded struct {
	SessionID  libacp.SessionID
	StopReason libacp.StopReason
}

// TurnFailed reports that a submitted prompt returned an error instead of a
// stop reason — a protocol error, a dead connection, or a transport-side
// failure. It is mutually exclusive with TurnEnded for the same submission.
type TurnFailed struct {
	SessionID libacp.SessionID
	Err       error
}

// ShellRunStarted reports that a `$`-style passthrough line was handed to
// the session's shell. Its output does not arrive here — it streams as
// TerminalChunk events.
type ShellRunStarted struct {
	SessionID libacp.SessionID
	Command   string
}

// ShellRunResult reports the outcome of one passthrough line. Snapshot is the
// scrollback the agent handed back with the run (often empty — live output
// comes as TerminalChunk). Err is ErrShellDisabled when the runtime was built
// without shell sessions, and non-nil otherwise only on a genuine failure.
type ShellRunResult struct {
	SessionID libacp.SessionID
	Offset    int64
	Started   bool
	Snapshot  string
	Err       error
}

// Event interface implementations. Kept in one block so the type definitions
// above stay readable; adding a type here without adding it to the translation
// table is what TestUnit_Translate_CoversEverySessionUpdateKind guards.

func (e TextDelta) SessionOf() libacp.SessionID            { return e.SessionID }
func (e ThoughtDelta) SessionOf() libacp.SessionID         { return e.SessionID }
func (e UserEcho) SessionOf() libacp.SessionID             { return e.SessionID }
func (e ToolCallOpened) SessionOf() libacp.SessionID       { return e.SessionID }
func (e ToolCallUpdated) SessionOf() libacp.SessionID      { return e.SessionID }
func (e PlanUpdated) SessionOf() libacp.SessionID          { return e.SessionID }
func (e UsageUpdated) SessionOf() libacp.SessionID         { return e.SessionID }
func (e CommandsUpdated) SessionOf() libacp.SessionID      { return e.SessionID }
func (e ConfigOptionUpdated) SessionOf() libacp.SessionID  { return e.SessionID }
func (e ModeUpdated) SessionOf() libacp.SessionID          { return e.SessionID }
func (e SessionInfoUpdated) SessionOf() libacp.SessionID   { return e.SessionID }
func (e MissionReport) SessionOf() libacp.SessionID        { return e.SessionID }
func (e MissionAsk) SessionOf() libacp.SessionID           { return e.SessionID }
func (e MissionStatusChanged) SessionOf() libacp.SessionID { return e.SessionID }
func (e MissionPlanRevised) SessionOf() libacp.SessionID   { return e.SessionID }

// SessionOf is the empty SessionID by construction — see InboxItemAdded.
func (InboxItemAdded) SessionOf() libacp.SessionID { return "" }

func (e TerminalChunk) SessionOf() libacp.SessionID       { return e.SessionID }
func (e UnknownUpdate) SessionOf() libacp.SessionID       { return e.SessionID }
func (e PermissionRequested) SessionOf() libacp.SessionID { return e.SessionID }
func (e PermissionResolved) SessionOf() libacp.SessionID  { return e.SessionID }
func (e TurnEnded) SessionOf() libacp.SessionID           { return e.SessionID }
func (e TurnFailed) SessionOf() libacp.SessionID          { return e.SessionID }
func (e ShellRunStarted) SessionOf() libacp.SessionID     { return e.SessionID }
func (e ShellRunResult) SessionOf() libacp.SessionID      { return e.SessionID }
func (e ReplayEnded) SessionOf() libacp.SessionID         { return e.SessionID }

func (ReplayEnded) isBridgeEvent()          {}
func (TextDelta) isBridgeEvent()            {}
func (ThoughtDelta) isBridgeEvent()         {}
func (UserEcho) isBridgeEvent()             {}
func (ToolCallOpened) isBridgeEvent()       {}
func (ToolCallUpdated) isBridgeEvent()      {}
func (PlanUpdated) isBridgeEvent()          {}
func (UsageUpdated) isBridgeEvent()         {}
func (CommandsUpdated) isBridgeEvent()      {}
func (ConfigOptionUpdated) isBridgeEvent()  {}
func (ModeUpdated) isBridgeEvent()          {}
func (SessionInfoUpdated) isBridgeEvent()   {}
func (MissionReport) isBridgeEvent()        {}
func (MissionAsk) isBridgeEvent()           {}
func (MissionStatusChanged) isBridgeEvent() {}
func (MissionPlanRevised) isBridgeEvent()   {}
func (InboxItemAdded) isBridgeEvent()       {}
func (TerminalChunk) isBridgeEvent()        {}
func (UnknownUpdate) isBridgeEvent()        {}
func (PermissionRequested) isBridgeEvent()  {}
func (PermissionResolved) isBridgeEvent()   {}
func (TurnEnded) isBridgeEvent()            {}
func (TurnFailed) isBridgeEvent()           {}
func (ShellRunStarted) isBridgeEvent()      {}
func (ShellRunResult) isBridgeEvent()       {}

const (
	// missionReportMetaKey and missionAskMetaKey are the `_meta` keys
	// reportrouter stamps onto the agent_message_chunk it delivers into a
	// mission's firing session; duplicated here because they're unexported
	// there, and this package's round-trip tests pin the wire contract.
	missionReportMetaKey = "contenox.missionReport"
	missionAskMetaKey    = "contenox.missionAsk"

	// missionStatusMetaKey and missionPlanMetaKey are the same device for
	// the mission lifecycle half: status changes and plan revisions.
	missionStatusMetaKey = "contenox.missionStatus"
	missionPlanMetaKey   = "contenox.missionPlan"
)

// missionReportMeta mirrors reportrouter's reportAttribution.
type missionReportMeta struct {
	MissionID string `json:"missionId"`
	ReportID  string `json:"reportId"`
	Kind      string `json:"kind"`
	AgentName string `json:"agentName,omitempty"`
}

// missionAskMeta mirrors reportrouter's askAttribution.
type missionAskMeta struct {
	MissionID string `json:"missionId"`
	AskID     string `json:"askId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
}

// missionStatusMeta mirrors the routed projection of
// missionservice.StatusChangedEvent. Intent is decoded but not yet rendered
// by any surface — it's already visible in the /mission line above it.
type missionStatusMeta struct {
	MissionID string `json:"missionId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	OldStatus string `json:"oldStatus"`
	NewStatus string `json:"newStatus"`
	Reason    string `json:"reason,omitempty"`
}

// missionPlanMeta mirrors the routed projection of
// missionservice.PlanRevisedEvent. Added/Removed is deliberately not part of
// this envelope — the counts below are the plan's current shape.
type missionPlanMeta struct {
	MissionID   string `json:"missionId"`
	AgentName   string `json:"agentName,omitempty"`
	Revision    int    `json:"revision"`
	Explanation string `json:"explanation,omitempty"`
	EntryCount  int    `json:"entryCount"`
	Pending     int    `json:"pending"`
	InProgress  int    `json:"inProgress"`
	Completed   int    `json:"completed"`
}

// terminalOutputMeta mirrors acpsvc's terminalOutputPayload.
type terminalOutputMeta struct {
	SessionID string `json:"sessionId"`
	Offset    int64  `json:"offset"`
	Chunk     string `json:"chunk"`
	Reset     bool   `json:"reset,omitempty"`
}

// metaEnvelope decodes an ACP `_meta` object into its namespaced keys.
// Malformed or absent `_meta` yields an empty map, never an error — an
// unrecognized namespace is not a reason to lose the update.
func metaEnvelope(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// translate turns one inbound session/update into exactly one Event. It is a
// pure function of the notification — no Bridge state, no I/O — which is what
// makes the whole vocabulary table-testable without a running engine.
func translate(n libacp.SessionNotification) Event {
	u := n.Update
	sid := n.SessionID

	switch u.SessionUpdate {
	case libacp.SessionUpdateUserMessageChunk:
		return UserEcho{SessionID: sid, MessageID: u.MessageID, Text: contentText(u.Content)}

	case libacp.SessionUpdateAgentMessageChunk:
		// Mission traffic rides this kind with a namespaced _meta envelope;
		// recognizing it here keeps "one notification, one event" true instead
		// of emitting TextDelta plus a second mission event. A stamped
		// namespace that fails to decode becomes UnknownUpdate rather than a
		// laundered TextDelta; an absent namespace stays plain text.
		env := metaEnvelope(u.Meta)
		if raw, ok := env[missionReportMetaKey]; ok {
			var m missionReportMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return UnknownUpdate{SessionID: sid, Kind: u.SessionUpdate, Update: u}
			}
			return MissionReport{
				SessionID: sid,
				MissionID: m.MissionID,
				ReportID:  m.ReportID,
				Kind:      m.Kind,
				AgentName: m.AgentName,
				MessageID: u.MessageID,
				Text:      contentText(u.Content),
			}
		}
		if raw, ok := env[missionStatusMetaKey]; ok {
			var m missionStatusMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return UnknownUpdate{SessionID: sid, Kind: u.SessionUpdate, Update: u}
			}
			return MissionStatusChanged{
				SessionID: sid,
				MissionID: m.MissionID,
				AgentName: m.AgentName,
				Old:       m.OldStatus,
				New:       m.NewStatus,
				Reason:    m.Reason,
				MessageID: u.MessageID,
			}
		}
		if raw, ok := env[missionPlanMetaKey]; ok {
			var m missionPlanMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return UnknownUpdate{SessionID: sid, Kind: u.SessionUpdate, Update: u}
			}
			return MissionPlanRevised{
				SessionID:   sid,
				MissionID:   m.MissionID,
				AgentName:   m.AgentName,
				Revision:    m.Revision,
				Explanation: m.Explanation,
				EntryCount:  m.EntryCount,
				Pending:     m.Pending,
				InProgress:  m.InProgress,
				Completed:   m.Completed,
				MessageID:   u.MessageID,
			}
		}
		if raw, ok := env[missionAskMetaKey]; ok {
			var m missionAskMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return UnknownUpdate{SessionID: sid, Kind: u.SessionUpdate, Update: u}
			}
			return MissionAsk{
				SessionID: sid,
				MissionID: m.MissionID,
				AskID:     m.AskID,
				AgentName: m.AgentName,
				Intent:    m.Intent,
				Summary:   m.Summary,
				Detail:    m.Detail,
				MessageID: u.MessageID,
				Text:      contentText(u.Content),
			}
		}
		return TextDelta{SessionID: sid, MessageID: u.MessageID, Text: contentText(u.Content)}

	case libacp.SessionUpdateAgentThoughtChunk:
		return ThoughtDelta{SessionID: sid, MessageID: u.MessageID, Text: contentText(u.Content)}

	case libacp.SessionUpdateToolCall:
		return ToolCallOpened{
			SessionID:  sid,
			ToolCallID: u.ToolCallID,
			Title:      u.Title,
			Kind:       u.Kind,
			Status:     u.Status,
			Contents:   u.ToolContent,
			Locations:  u.Locations,
			RawInput:   u.RawInput,
		}

	case libacp.SessionUpdateToolCallUpdate:
		return ToolCallUpdated{
			SessionID:  sid,
			ToolCallID: u.ToolCallID,
			Title:      u.Title,
			Kind:       u.Kind,
			Status:     u.Status,
			Contents:   u.ToolContent,
			Locations:  u.Locations,
			RawInput:   u.RawInput,
			RawOutput:  u.RawOutput,
		}

	case libacp.SessionUpdatePlan:
		return PlanUpdated{SessionID: sid, Entries: u.Entries}

	case libacp.SessionUpdateAvailableCommands:
		return CommandsUpdated{SessionID: sid, Commands: u.AvailableCommands}

	case libacp.SessionUpdateCurrentMode:
		return ModeUpdated{SessionID: sid, ModeID: u.CurrentModeID}

	case libacp.SessionUpdateConfigOption:
		return ConfigOptionUpdated{SessionID: sid, Options: u.ConfigOptions}

	case libacp.SessionUpdateUsageUpdate:
		return UsageUpdated{SessionID: sid, Used: u.Used, Size: u.Size, Cost: u.Cost}

	case libacp.SessionUpdateSessionInfo:
		return SessionInfoUpdated{SessionID: sid, Title: u.Title, UpdatedAt: u.UpdatedAt}

	case acpsvc.TerminalOutputUpdateKind:
		if raw, ok := metaEnvelope(u.Meta)[acpsvc.TerminalOutputMetaKey]; ok {
			var p terminalOutputMeta
			if err := json.Unmarshal(raw, &p); err == nil {
				return TerminalChunk{SessionID: sid, Offset: p.Offset, Chunk: p.Chunk, Reset: p.Reset}
			}
		}
		// The discriminator says terminal output but the payload is missing or
		// malformed: hand it through raw rather than invent an empty chunk that
		// a Reset consumer would act on by clearing its buffer.
		return UnknownUpdate{SessionID: sid, Kind: u.SessionUpdate, Update: u}
	}

	return UnknownUpdate{SessionID: sid, Kind: u.SessionUpdate, Update: u}
}

// contentText projects a content block down to its text. A nil block (every
// non-content update kind) and a non-text block both read as "".
func contentText(c *libacp.ContentBlock) string {
	if c == nil {
		return ""
	}
	return c.Text
}
