package approvalflow

import (
	"encoding/json"
	"strings"

	"github.com/contenox/libacp"
)

// Mapped is an inbound ACP permission request projected onto hitlservice's
// evaluator inputs, with honesty flags for how much of it is real.
type Mapped struct {
	// ToolsName and ToolName are the contenox tool identity; meaningful only when Named.
	ToolsName string
	ToolName  string

	// Args is the decoded rawInput; meaningful as call arguments only when ArgsKnown.
	Args map[string]any

	// Named reports both names were recovered from the `_meta` envelope.
	Named bool

	// ArgsKnown reports the request carried a decodable rawInput object.
	ArgsKnown bool

	// PolicyName is the policy the downstream evaluated before asking; display/audit only.
	PolicyName string

	// Diff is the downstream's rendered unified diff, when supplied.
	Diff string

	// MayCall and MayCallDeclared are the declared reach the downstream sent; see Meta.MayCall.
	MayCall         []string
	MayCallDeclared *bool

	// Title is the human-facing label, falling back to the tool-call id.
	Title string

	// ToolCallID is the downstream's own id for the gated call.
	ToolCallID string
}

// MapRequest projects req onto hitlservice's evaluator inputs. Names come
// only from the `_meta` envelope, never guessed; unnamed means "requires approval".
func MapRequest(req libacp.RequestPermissionRequest) Mapped {
	m := Mapped{
		Args:       map[string]any{},
		Title:      strings.TrimSpace(req.ToolCall.Title),
		ToolCallID: req.ToolCall.ToolCallID,
	}
	if m.Title == "" {
		m.Title = req.ToolCall.ToolCallID
	}

	// The request-level envelope wins over the tool-call one.
	meta, ok := ParseMeta(req.Meta)
	if !ok {
		meta, _ = ParseMeta(req.ToolCall.Meta)
	}
	m.ToolsName = strings.TrimSpace(meta.ToolsName)
	m.ToolName = strings.TrimSpace(meta.ToolName)
	m.Named = m.ToolsName != "" && m.ToolName != ""
	m.PolicyName = strings.TrimSpace(meta.PolicyName)
	m.Diff = meta.Diff
	m.MayCall = meta.MayCall
	m.MayCallDeclared = meta.MayCallDeclared

	if args, ok := ParseArgs(req.ToolCall.RawInput); ok {
		m.Args = args
		m.ArgsKnown = true
	}
	return m
}

// ParseMeta decodes the `_meta` envelope; reports false if absent, malformed, or all-empty.
func ParseMeta(raw json.RawMessage) (Meta, bool) {
	if len(raw) == 0 {
		return Meta{}, false
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Meta{}, false
	}
	if meta.IsZero() {
		return Meta{}, false
	}
	return meta, true
}

// ParseArgs decodes rawInput as an argument map, or reports false if it isn't a JSON object.
func ParseArgs(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, false
	}
	if args == nil {
		return nil, false
	}
	return args, true
}

// SelectOption picks the option id of the given polarity, once-kind before
// always-kind, or false if none of that polarity was offered.
func SelectOption(req libacp.RequestPermissionRequest, allow bool) (string, bool) {
	wanted := [2]libacp.PermissionOptionKind{libacp.PermissionRejectOnce, libacp.PermissionRejectAlways}
	if allow {
		wanted = [2]libacp.PermissionOptionKind{libacp.PermissionAllowOnce, libacp.PermissionAllowAlways}
	}
	for _, kind := range wanted {
		for _, opt := range req.Options {
			if opt.Kind == kind && opt.OptionID != "" {
				return opt.OptionID, true
			}
		}
	}
	return "", false
}

// Answer builds the response that grants or refuses req, degrading to
// "cancelled" rather than inventing an option id the downstream never offered.
func Answer(req libacp.RequestPermissionRequest, allow bool) libacp.RequestPermissionResponse {
	if id, ok := SelectOption(req, allow); ok {
		return libacp.RequestPermissionResponse{
			Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeSelected, OptionID: id},
		}
	}
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
	}
}
