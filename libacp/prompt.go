package libacp

import "encoding/json"

type StopReason string

const (
	StopReasonEndTurn         StopReason = "end_turn"
	StopReasonMaxTokens       StopReason = "max_tokens"
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	StopReasonRefusal         StopReason = "refusal"
	StopReasonCancelled       StopReason = "cancelled"
)

type PromptRequest struct {
	SessionID SessionID       `json:"sessionId"`
	Prompt    []ContentBlock  `json:"prompt"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// PromptResponse is the result of a "session/prompt" request. Per the ACP v1
// schema it carries only stopReason and _meta — a prior revision added a
// non-spec "usage" field (per-turn token counts) directly at the type's
// root, which extensibility.mdx forbids: "Implementations MUST NOT add any
// custom fields at the root of a type that's part of the specification."
// It was removed rather than migrated into _meta because nothing in this
// repo produced or consumed it: acpsvc never populated it, and the beam
// client only ever destructured stopReason from the call result. Session
// context/cost reporting already has a sanctioned, fully wired channel — the
// "usage_update" SessionUpdate (see SessionUpdateUsageUpdate) — which is
// where that data belongs and is already emitted (see acpsvc's
// sendInitialUsageUpdate and its translateEvents usage_update path).
type PromptResponse struct {
	StopReason StopReason      `json:"stopReason"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type SessionUpdateKind string

const (
	SessionUpdateUserMessageChunk  SessionUpdateKind = "user_message_chunk"
	SessionUpdateAgentMessageChunk SessionUpdateKind = "agent_message_chunk"
	SessionUpdateAgentThoughtChunk SessionUpdateKind = "agent_thought_chunk"
	SessionUpdateToolCall          SessionUpdateKind = "tool_call"
	SessionUpdateToolCallUpdate    SessionUpdateKind = "tool_call_update"
	SessionUpdatePlan              SessionUpdateKind = "plan"
	SessionUpdateAvailableCommands SessionUpdateKind = "available_commands_update"
	SessionUpdateCurrentMode       SessionUpdateKind = "current_mode_update"
	SessionUpdateConfigOption      SessionUpdateKind = "config_option_update"
	SessionUpdateUsageUpdate       SessionUpdateKind = "usage_update"
	SessionUpdateSessionInfo       SessionUpdateKind = "session_info_update"
)

// AllSessionUpdateKinds returns every SessionUpdateKind the spec defines, in
// declaration order. It exists so that a consumer's translation table can be
// tested for COMPLETENESS against the library rather than against a second,
// hand-maintained copy of this list: a client that maps update kinds to its own
// vocabulary (see internal/surfaces/beamtui/enginebridge) iterates this and
// fails when a kind has no arm, instead of silently degrading the new kind to
// its unknown-update fallback for as long as nobody notices.
//
// That makes it part of the contract: ADDING A CONST ABOVE MEANS ADDING IT
// HERE. The slice is freshly built on every call, so a caller cannot corrupt
// the answer for the next one.
func AllSessionUpdateKinds() []SessionUpdateKind {
	return []SessionUpdateKind{
		SessionUpdateUserMessageChunk,
		SessionUpdateAgentMessageChunk,
		SessionUpdateAgentThoughtChunk,
		SessionUpdateToolCall,
		SessionUpdateToolCallUpdate,
		SessionUpdatePlan,
		SessionUpdateAvailableCommands,
		SessionUpdateCurrentMode,
		SessionUpdateConfigOption,
		SessionUpdateUsageUpdate,
		SessionUpdateSessionInfo,
	}
}

type SessionUpdate struct {
	SessionUpdate SessionUpdateKind `json:"sessionUpdate"`

	Content *ContentBlock `json:"-"`

	ToolCallID  string             `json:"toolCallId,omitempty"`
	Title       string             `json:"title,omitempty"`
	Kind        ToolKind           `json:"kind,omitempty"`
	Status      ToolCallStatus     `json:"status,omitempty"`
	ToolContent []ToolCallContent  `json:"-"`
	Locations   []ToolCallLocation `json:"locations,omitempty"`
	RawInput    json.RawMessage    `json:"rawInput,omitempty"`
	RawOutput   json.RawMessage    `json:"rawOutput,omitempty"`

	Entries []PlanEntry `json:"entries,omitempty"`

	AvailableCommands []AvailableCommand `json:"availableCommands,omitempty"`

	CurrentModeID string `json:"currentModeId,omitempty"`

	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`

	// For usage_update (ACP session context indicator)
	Used int        `json:"used,omitempty"`
	Size int        `json:"size,omitempty"`
	Cost *UsageCost `json:"cost,omitempty"`

	// MessageID groups streamed chunks into messages: all chunks of one message
	// share an id; a change marks a new message. Optional in the spec.
	MessageID string `json:"messageId,omitempty"`

	// UpdatedAt is the session_info_update timestamp (ISO 8601); Title above is
	// shared with tool_call updates (same wire key).
	UpdatedAt string `json:"updatedAt,omitempty"`

	Meta json.RawMessage `json:"_meta,omitempty"`
}

type sessionUpdateWire struct {
	SessionUpdate SessionUpdateKind `json:"sessionUpdate"`

	Content json.RawMessage `json:"content,omitempty"`

	ToolCallID string             `json:"toolCallId,omitempty"`
	Title      string             `json:"title,omitempty"`
	Kind       ToolKind           `json:"kind,omitempty"`
	Status     ToolCallStatus     `json:"status,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage    `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage    `json:"rawOutput,omitempty"`

	Entries []PlanEntry `json:"entries,omitempty"`

	// AvailableCommands is a pointer: the spec REQUIRES it on
	// available_commands_update (an empty list must still reach the wire as
	// `[]`, not be omitted — omitempty on a plain slice can't tell "empty" from
	// "absent"), while every other update kind must omit it entirely.
	AvailableCommands *[]AvailableCommand `json:"availableCommands,omitempty"`

	CurrentModeID string `json:"currentModeId,omitempty"`

	// ConfigOptions is a pointer for the same reason as AvailableCommands
	// above, required on config_option_update.
	ConfigOptions *[]SessionConfigOption `json:"configOptions,omitempty"`

	// Pointers: the spec REQUIRES used and size on usage_update (zero values
	// must reach the wire there), while every other update kind must omit them.
	Used *int       `json:"used,omitempty"`
	Size *int       `json:"size,omitempty"`
	Cost *UsageCost `json:"cost,omitempty"`

	MessageID string `json:"messageId,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`

	Meta json.RawMessage `json:"_meta,omitempty"`
}

type UsageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

func (u SessionUpdate) MarshalJSON() ([]byte, error) {
	w := sessionUpdateWire{
		SessionUpdate: u.SessionUpdate,
		ToolCallID:    u.ToolCallID,
		Title:         u.Title,
		Kind:          u.Kind,
		Status:        u.Status,
		Locations:     u.Locations,
		RawInput:      u.RawInput,
		RawOutput:     u.RawOutput,
		Entries:       u.Entries,
		CurrentModeID: u.CurrentModeID,
		Cost:          u.Cost,
		MessageID:     u.MessageID,
		UpdatedAt:     u.UpdatedAt,
		Meta:          u.Meta,
	}
	if u.SessionUpdate == SessionUpdateUsageUpdate {
		used, size := u.Used, u.Size
		w.Used, w.Size = &used, &size
	}
	if u.SessionUpdate == SessionUpdateAvailableCommands {
		commands := u.AvailableCommands
		if commands == nil {
			commands = []AvailableCommand{}
		}
		w.AvailableCommands = &commands
	}
	if u.SessionUpdate == SessionUpdateConfigOption {
		options := u.ConfigOptions
		if options == nil {
			options = []SessionConfigOption{}
		}
		w.ConfigOptions = &options
	}
	switch u.SessionUpdate {
	case SessionUpdateToolCall, SessionUpdateToolCallUpdate:
		if len(u.ToolContent) > 0 {
			raw, err := json.Marshal(u.ToolContent)
			if err != nil {
				return nil, err
			}
			w.Content = raw
		}
	default:
		if u.Content != nil {
			raw, err := json.Marshal(u.Content)
			if err != nil {
				return nil, err
			}
			w.Content = raw
		}
	}
	return json.Marshal(w)
}

func (u *SessionUpdate) UnmarshalJSON(data []byte) error {
	var w sessionUpdateWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*u = SessionUpdate{
		SessionUpdate: w.SessionUpdate,
		ToolCallID:    w.ToolCallID,
		Title:         w.Title,
		Kind:          w.Kind,
		Status:        w.Status,
		Locations:     w.Locations,
		RawInput:      w.RawInput,
		RawOutput:     w.RawOutput,
		Entries:       w.Entries,
		CurrentModeID: w.CurrentModeID,
		Cost:          w.Cost,
		MessageID:     w.MessageID,
		UpdatedAt:     w.UpdatedAt,
		Meta:          w.Meta,
	}
	if w.Used != nil {
		u.Used = *w.Used
	}
	if w.Size != nil {
		u.Size = *w.Size
	}
	if w.AvailableCommands != nil {
		u.AvailableCommands = *w.AvailableCommands
	}
	if w.ConfigOptions != nil {
		u.ConfigOptions = *w.ConfigOptions
	}
	if len(w.Content) == 0 {
		return nil
	}
	switch w.SessionUpdate {
	case SessionUpdateToolCall, SessionUpdateToolCallUpdate:
		return json.Unmarshal(w.Content, &u.ToolContent)
	default:
		var cb ContentBlock
		if err := json.Unmarshal(w.Content, &cb); err != nil {
			return err
		}
		u.Content = &cb
	}
	return nil
}

type SessionNotification struct {
	SessionID SessionID       `json:"sessionId"`
	Update    SessionUpdate   `json:"update"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

func NewAgentMessageChunk(text string) SessionUpdate {
	c := NewTextContent(text)
	return SessionUpdate{SessionUpdate: SessionUpdateAgentMessageChunk, Content: &c}
}

func NewAgentThoughtChunk(text string) SessionUpdate {
	c := NewTextContent(text)
	return SessionUpdate{SessionUpdate: SessionUpdateAgentThoughtChunk, Content: &c}
}

func NewUserMessageChunk(text string) SessionUpdate {
	c := NewTextContent(text)
	return SessionUpdate{SessionUpdate: SessionUpdateUserMessageChunk, Content: &c}
}
