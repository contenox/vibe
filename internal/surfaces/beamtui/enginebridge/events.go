package enginebridge

import (
	"encoding/json"

	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/surfaces/acpsvc"
	libacp "github.com/contenox/beam/libacp"
)

// Event is one fact produced by the runtime, already destructured out of the
// ACP wire types a UI has no business re-parsing. The set is CLOSED: only the
// types in this file implement it.
//
// # What "no drops" covers
//
// Exactly one Event is produced per inbound libacp.SessionNotification THAT
// REACHES THE BRIDGE'S CLIENT — which is the set the active-session filter
// admits, not everything on the wire. SetActiveSession installs a
// libacp.FilterSessionUpdates wrapper, and updates for any other session are
// discarded before translation ever runs; that is the point of the filter, and
// it is a drop by design. Within the admitted set the guarantee is total: no
// coalescing, no reordering, and a kind this package does not model becomes
// UnknownUpdate rather than nothing, so the contract survives protocol
// additions.
//
// The out-of-band events — permission, turn and shell results — do not ride the
// notification stream at all and are not filtered.
//
// # Routing
//
// Every Event carries the ACP session it belongs to, so a consumer can route
// without a side table. That does NOT make the Bridge a multiplexer: with a
// non-empty active session the filter admits one session's updates, so a
// surface that wants several live transcripts at once either keeps the filter
// off (SetActiveSession "", every session delivered, tagged) or runs one Bridge
// per session. SessionOf is for correct attribution — of the unfiltered window,
// of a late event from a session just switched away from — not a fan-out
// facility.
type Event interface {
	// SessionOf reports the ACP session this fact belongs to.
	SessionOf() libacp.SessionID
	isBridgeEvent()
}

// TextDelta is one streamed chunk of assistant prose (agent_message_chunk).
// MessageID groups chunks into a message: all chunks of one message share an
// id, and a changed id marks a new message. It is optional in the spec, so an
// empty MessageID means "the agent did not group this".
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
// transcript replay session/load performs, not an echo of the line the
// operator just typed (the Bridge never synthesizes one; SubmitPrompt's text
// is already in the caller's hands).
type UserEcho struct {
	SessionID libacp.SessionID
	MessageID string
	Text      string
}

// ToolCallOpened is the FIRST notification for a tool call (tool_call): the
// create-shaped one. A later state change for the same ToolCallID arrives as
// ToolCallUpdated.
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
// (tool_call_update). Fields the agent did not restate arrive zero — the
// contract is patch-shaped, so a renderer merges onto the card it already has
// rather than replacing it.
//
// An empty field ALWAYS means "unchanged", never "cleared". Clear-on-empty is
// inexpressible on this wire: the update encodes omitted fields with omitempty,
// so "" and absent are the same bytes, and there is no sentinel for erasure. A
// renderer that treated an empty Title or a nil Contents as a clear would blank
// cards on every partial update. Nothing can ever remove content from an open
// tool call — only add to or replace it with something non-empty.
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

// UsageUpdated is the session context indicator (usage_update). It is the ONLY
// channel for token accounting — PromptResponse carries a stop reason and
// nothing else.
//
// Size == 0 IS A REACHABLE WIRE SHAPE, not a bug and not "no data": the spec
// requires used and size on every usage_update, and a session running without a
// configured context budget reports its consumption against a size of zero.
// Consumers must therefore render Used ABSOLUTELY ("12,481 tokens") and never
// as a percentage of Size — the obvious Used/Size divides by zero, and clamping
// it to 100% would report a session with no limit as full. A meter is only
// honest when Size > 0.
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
// workspace root. Options replace the previous list.
//
// It arrives from two places, and a consumer must not care which. The wire
// notification is one: acpsvc pushes it after any command that changes a
// selection (/model, /provider, /think, /policy) and after set_config_option.
// The other is the session's OPENING options, which ride the session/new,
// session/load and session/resume RESPONSES rather than a notification —
// NewSession/LoadSession/ResumeSession replay them as this event so a consumer
// that only listens on Events() still sees the session's starting state. See
// (*Bridge).emitInitialConfigOptions.
type ConfigOptionUpdated struct {
	SessionID libacp.SessionID
	Options   []libacp.SessionConfigOption
}

// ValueDomains projects config options onto the argument domains of the slash
// commands that take a value — /model, /provider, /think, /policy — so a
// completing surface offers exactly what the server advertises and validates.
// See acpsvc.CommandValueDomains for the mapping and its rules; this is the
// re-export that keeps the UI layer's imports pointed at the bridge.
//
// An absent key means "no domain known", which a caller must treat as "anything
// the operator types is fine" — never as a gate.
func ValueDomains(options []libacp.SessionConfigOption) map[string][]string {
	return acpsvc.CommandValueDomains(options)
}

// ModeUpdated reports the session's current mode id (current_mode_update).
type ModeUpdated struct {
	SessionID libacp.SessionID
	ModeID    string
}

// ReplayEnded marks the end of a LoadSession transcript replay. It is a
// bridge-synthesized event, not a wire fact: the replayed notifications are
// dispatched before the load RPC's response, so emitting this after the
// response preserves order — everything the replay produced precedes it.
// Consumers settle the trailing replayed message on it; a session that was
// never loaded never sees one.
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

// MissionReport is a report from a dispatched mission unit, delivered into the
// session that FIRED it. On the wire it is an agent_message_chunk whose
// MessageID is "mission-report-<reportId>" and whose _meta carries
// reportrouter's contenox.missionReport envelope; the Bridge recognizes it and
// emits this INSTEAD OF TextDelta, so one notification stays one event.
type MissionReport struct {
	SessionID libacp.SessionID
	MissionID string
	ReportID  string
	Kind      string
	AgentName string
	MessageID string
	Text      string
}

// MissionAsk is an attention question from a dispatched mission unit: the unit
// is BLOCKED until it is answered. Same shape as MissionReport — an
// agent_message_chunk with MessageID "mission-ask-<askId>" and a
// contenox.missionAsk _meta envelope — and likewise emitted instead of
// TextDelta. AskID is what an answer is given against.
//
// Answering is NOT a Bridge method yet: the answer path is a service seam the
// blueprint leaves to the approval-cards component (D20).
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
// announced into the session that FIRED it. Same carrier as MissionReport: an
// agent_message_chunk whose MessageID is "mission-status-<missionId>-<n>" and
// whose _meta carries the contenox.missionStatus envelope, emitted INSTEAD OF
// TextDelta so one notification stays one event.
//
// Old and New are missionservice.Status values as STRINGS, deliberately not a
// typed enum: this package models the wire, and the wire carries whatever the
// service published. The vocabulary is one running state ("open") and four
// terminal ones ("landed", "derailed", "stuck", "abandoned"); a consumer that
// switches on New must have a default arm, because a status this build does not
// know is a service that grew one, not a fact to drop.
//
// Reason is the one line the finisher gave — why it derailed, what wedged it —
// and is empty for a clean landing.
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
// announced into the firing session. Carrier and rules are MissionStatusChanged's
// — an agent_message_chunk with MessageID "mission-plan-<missionId>-<revision>"
// and a contenox.missionPlan envelope.
//
// Revision is the plan's monotonic number and Explanation the planner's one-line
// "why". EntryCount is the whole plan's size; Pending, InProgress and Completed
// break it down. The three counts DO NOT have to sum to EntryCount — the plan's
// status vocabulary may grow — so a renderer shows them as the three it knows
// and never derives a fourth by subtraction.
//
// A plan revision is NOT an attention event: it is a unit reorganizing its own
// work, which is exactly the thing the operator delegated. See the app-shell's
// bell rules.
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
// mirroring missionservice.Status (internal/services/missionservice: StatusOpen
// and the four terminal constants). Duplicated here rather than imported for the
// same reason the `_meta` keys are: this package models the WIRE, and a surface
// that switches on a status should not have to reach past the bridge to name it.
//
// The set is closed BY CONVENTION, not by a Go type — see MissionStatusChanged
// on why every consumer needs a default arm.
const (
	MissionStatusOpen      = "open"
	MissionStatusLanded    = "landed"
	MissionStatusDerailed  = "derailed"
	MissionStatusStuck     = "stuck"
	MissionStatusAbandoned = "abandoned"
)

// MissionStatusTerminal reports whether status is one a mission comes to REST
// in: the work is over, one way or another, and nothing further will arrive from
// that unit. "open" is not terminal, and neither is a status this build does not
// know — an unrecognized value is treated as still-running, which is the answer
// that cannot invent a completion that did not happen.
//
// It is the closed set the completion bell rings on (blueprint 4.20): a mission
// reaching rest is precisely the moment detached work has something to hand back.
func MissionStatusTerminal(status string) bool {
	switch status {
	case MissionStatusLanded, MissionStatusDerailed, MissionStatusStuck, MissionStatusAbandoned:
		return true
	}
	return false
}

// InboxItemAdded is a mission report that reached NO live supervising session
// and was written to the durable operator inbox instead
// (internal/services/operatorinbox). It is the ONE event in this vocabulary that
// does not come off the ACP wire: it arrives on the process bus, because by
// definition there was no session to deliver it into.
//
// # It belongs to no session
//
// SessionOf therefore returns the EMPTY SessionID, always. That is not a missing
// value to be filled in later — it is the fact the event carries: nobody was
// watching. A consumer must not route it by session (there is nothing to route
// to) and must not let a session filter drop it; it is a surface-level notice,
// the same way a connection warning is.
//
// Fields are the inbox Item's own, flattened: ID identifies the durable row,
// Reason is why it landed here ("operator_fired" — nobody was ever supervising;
// "parent_gone" — the supervisor ended before the report arrived), and the rest
// is the attribution a one-line notice needs without a second read.
type InboxItemAdded struct {
	ID        string
	MissionID string
	AgentName string
	Intent    string
	Reason    string
	// Kind and Summary are the embedded report's own: the report kind
	// ("progress", "finding", "blocker", "result") and its one-line summary.
	Kind    string
	Summary string
}

// TerminalChunk is live output from the session's persistent shell, carried by
// acpsvc's extension update kind. Reset marks the scrollback snapshot
// delivered on (re)subscribe: the consumer must REPLACE its buffer, not append
// to it. Offset is the byte offset of Chunk within that scrollback.
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

// PermissionRequested is a HITL gate: the tool call named here is BLOCKED
// until Resolve is called. It blocks only that call — the rest of the turn's
// stream keeps flowing, and other sessions are untouched.
//
// Resolve is idempotent and safe to call from any goroutine; the first call
// wins and later ones are no-ops. allow=true answers with the "allow" option,
// false with "deny" — the only two options approvalflow ever offers. A request
// left unresolved when the Bridge closes (or when the turn is cancelled)
// resolves itself as cancelled, which acpsvc maps to context.Canceled: no
// goroutine survives teardown waiting for a keystroke.
//
// However it ends, the end is announced: see PermissionResolved, which is what
// a card should retire on.
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
	// Options is what the agent offered, verbatim. approvalflow always sends
	// exactly [allow(allow_once), deny(reject_once)]; it is carried so a card
	// can label its keys from the wire instead of hardcoding them.
	Options []libacp.PermissionOption
	Resolve func(allow bool)
}

// PermissionResolved is emitted when a pending permission request reaches ANY
// terminal state — the operator answered, the turn was cancelled and libacp
// force-resolved the request, or the bridge tore down — so an approval card can
// retire DETERMINISTICALLY instead of being garbage-collected by inference.
// Exactly one PermissionResolved follows each PermissionRequested, matched on
// ToolCallID.
//
// Outcome is the answer that went back on the wire: PermissionOutcomeSelected
// when the operator chose (the choice itself — allow or deny — is the tool
// call's business and surfaces as its status, not here), PermissionOutcomeCancelled
// for both non-answers.
//
// The one case where it does NOT arrive is teardown: the queue is already
// stopped when the bridge's done channel closes, so that emit is dropped. The
// consumer learns the same fact more strongly — Events() closes in the same
// instant, retiring every open card at once.
type PermissionResolved struct {
	SessionID  libacp.SessionID
	ToolCallID string
	Outcome    libacp.PermissionOutcomeKind
}

// TurnEnded reports that a submitted prompt finished. A genuine cancel lands
// here with StopReason "cancelled" and is NOT an error; TurnFailed means the
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

// ShellRunStarted reports that a `$`-style passthrough line was handed to the
// session's shell. Its output does NOT arrive here — it streams as
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
	// missionReportMetaKey and missionAskMetaKey are the `_meta` keys the
	// report router stamps onto the agent_message_chunk it delivers into a
	// mission's firing session (internal/services/reportrouter/reportrouter.go:
	// reportUpdateMeta / askUpdateMeta). They are unexported there, so the
	// literals are duplicated here — the wire is the contract, and the
	// round-trip tests in this package pin it.
	missionReportMetaKey = "contenox.missionReport"
	missionAskMetaKey    = "contenox.missionAsk"

	// missionStatusMetaKey and missionPlanMetaKey are the same device for the
	// mission LIFECYCLE half: the report router stamps them onto the
	// agent_message_chunk it delivers when a mission changes status
	// (missionservice.StatusChangedEvent, published on
	// missionservice.StatusChangedSubject) or replaces its plan
	// (missionservice.PlanRevisedEvent, on PlanRevisedSubject). Same duplication
	// rule as the two keys above, for the same reason: the producer's constants
	// are unexported, the WIRE is the contract, and the translate table in this
	// package's tests is what pins it.
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
// missionservice.StatusChangedEvent. Intent rides the wire (it is what the
// mission was FOR) and is decoded so the shape stays honest, but no beam surface
// renders it on a status line yet: the card names the unit, and the intent is
// already in the transcript above it, in the /mission line that fired the work.
type missionStatusMeta struct {
	MissionID string `json:"missionId"`
	AgentName string `json:"agentName,omitempty"`
	Intent    string `json:"intent,omitempty"`
	OldStatus string `json:"oldStatus"`
	NewStatus string `json:"newStatus"`
	Reason    string `json:"reason,omitempty"`
}

// missionPlanMeta mirrors the routed projection of
// missionservice.PlanRevisedEvent. The event's Added/Removed delta is
// deliberately NOT part of this envelope — the counts below are the plan's
// current shape, which is what a one-line card can render honestly at any
// width.
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

// metaEnvelope decodes an ACP `_meta` object into its top-level namespaced
// keys without committing to any one of them. Malformed or absent `_meta`
// yields an empty map, never an error: a namespace a producer did not stamp is
// indistinguishable from one this build does not know about, and neither is a
// reason to lose the update.
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
		// Mission traffic rides this kind with a namespaced _meta envelope.
		// Recognizing it here — rather than emitting TextDelta AND a second
		// mission event — is what keeps "one notification, one event" true.
		// A namespace the producer STAMPED but whose payload will not decode is
		// a broken producer, not prose. Falling back to TextDelta there would
		// launder a mission report into an ordinary assistant message —
		// unattributed, unanswerable, and indistinguishable from something the
		// model said. UnknownUpdate hands the raw update through instead, which
		// is the same policy the terminal-extension arm below applies to the
		// same failure. An ABSENT namespace stays plain text: that is a chunk
		// nobody claimed, which is exactly what a TextDelta is.
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
