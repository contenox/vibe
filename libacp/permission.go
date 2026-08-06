package libacp

import (
	"encoding/json"
	"fmt"
)

type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

type PermissionOption struct {
	OptionID string               `json:"optionId"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
	Meta     json.RawMessage      `json:"_meta,omitempty"`
}

type RequestPermissionRequest struct {
	SessionID SessionID          `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
	Meta      json.RawMessage    `json:"_meta,omitempty"`
}

type PermissionToolCall struct {
	ToolCallID string             `json:"toolCallId"`
	Title      string             `json:"title,omitempty"`
	Kind       ToolKind           `json:"kind,omitempty"`
	Status     ToolCallStatus     `json:"status,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage    `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage    `json:"rawOutput,omitempty"`
	Meta       json.RawMessage    `json:"_meta,omitempty"`
}

type PermissionOutcomeKind string

const (
	PermissionOutcomeCancelled PermissionOutcomeKind = "cancelled"
	PermissionOutcomeSelected  PermissionOutcomeKind = "selected"
)

type RequestPermissionOutcome struct {
	Outcome  PermissionOutcomeKind `json:"outcome"`
	OptionID string                `json:"optionId,omitempty"`
}

func (o *RequestPermissionOutcome) UnmarshalJSON(data []byte) error {
	var probe struct {
		Outcome PermissionOutcomeKind `json:"outcome"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("libacp: permission outcome: %w", err)
	}
	switch probe.Outcome {
	case PermissionOutcomeCancelled:
		o.Outcome = PermissionOutcomeCancelled
		o.OptionID = ""
		return nil
	case PermissionOutcomeSelected:
		var sel struct {
			Outcome  PermissionOutcomeKind `json:"outcome"`
			OptionID string                `json:"optionId"`
		}
		if err := json.Unmarshal(data, &sel); err != nil {
			return err
		}
		o.Outcome = PermissionOutcomeSelected
		o.OptionID = sel.OptionID
		return nil
	}
	return fmt.Errorf("libacp: unknown permission outcome %q", probe.Outcome)
}

type RequestPermissionResponse struct {
	Outcome RequestPermissionOutcome `json:"outcome"`
	Meta    json.RawMessage          `json:"_meta,omitempty"`
}
