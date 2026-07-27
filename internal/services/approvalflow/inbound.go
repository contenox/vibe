package approvalflow

import (
	"encoding/json"
	"strings"

	"github.com/contenox/beam/libacp"
)

// This file is the INVERSE of BuildRequest: it projects an inbound ACP
// session/request_permission back onto the inputs hitlservice's evaluator wants
// — (toolsName, toolName, args). It lives beside the outbound builder on
// purpose, so the two halves of one wire contract cannot drift apart.
//
// # Why a projection is needed at all
//
// hitlservice.PolicyEvaluator.Evaluate takes a CONTENOX tool call: a tools
// namespace, a tool within it, and the call's argument map. An ACP permission
// request is a different shape — a tool-call id, a human-facing title, an ACP
// ToolKind, and an opaque rawInput blob, all authored by whatever agent is on
// the other end of the connection. Nothing guarantees the two vocabularies
// meet: a foreign agent's "Write" is not necessarily contenox's
// local_fs.write_file, and no amount of string munging can make it so
// truthfully.
//
// # The mapping
//
//   - toolsName / toolName come ONLY from the `_meta` envelope this package's
//     own BuildRequest writes (Meta.ToolsName / Meta.ToolName), read from the
//     request first and the tool call second. That envelope is present exactly
//     when the downstream is a contenox runtime (a chain unit, or a nested
//     `contenox acp`), which is exactly when the names mean what a contenox
//     policy means by them.
//   - args come from toolCall.rawInput when it decodes to a JSON object — the
//     same bytes MarshalArgs wrote on the way out.
//
// # What is deliberately NOT done
//
// The tool-call id is NOT split on "." to recover names, and the title is NOT
// parsed. Both would sometimes produce a plausible-looking (toolsName,
// toolName) pair for an agent that never spoke contenox's vocabulary — and a
// fabricated pair can MATCH AN ALLOW RULE, turning a mapping gap into silent
// permission. The failure direction of a guess here is unsafe, so there is no
// guess: a request without the envelope is reported unnamed, and a caller must
// treat that as "requires a human", never as "permitted".
//
// Similarly, an absent rawInput is reported as args-unknown rather than as an
// empty argument map. The difference is load-bearing: a policy's condition-
// bearing DENY rules cannot match against arguments that are not there, so an
// "allow" verdict reached without arguments is not a verdict about this call —
// it is a verdict about a call with no arguments, which this may not be.

// Mapped is an inbound ACP permission request projected onto hitlservice's
// evaluator inputs, with the two honesty flags a caller needs to know how much
// of the projection is real. See the file comment for the rules.
type Mapped struct {
	// ToolsName and ToolName are the contenox tool identity, or "" when the
	// request carried no contenox `_meta` envelope. Meaningful only when Named
	// is true.
	ToolsName string
	ToolName  string

	// Args is the decoded rawInput, or an empty (non-nil) map when the request
	// carried none. Meaningful as the CALL's arguments only when ArgsKnown.
	Args map[string]any

	// Named reports that both ToolsName and ToolName were recovered from the
	// `_meta` envelope. False means the request is UNMAPPABLE onto a contenox
	// policy: evaluating one anyway would be evaluating a rule set against a
	// vocabulary it was not written for.
	Named bool

	// ArgsKnown reports that the request carried a decodable rawInput object.
	// False means condition-bearing rules could not have been evaluated
	// against real arguments, so an allow verdict must not be trusted.
	ArgsKnown bool

	// PolicyName is the policy the DOWNSTREAM evaluated before deciding to ask
	// (Meta.PolicyName), when it said. It is provenance for display and audit —
	// the asking side's policy, NOT the envelope the receiving side applies —
	// and nothing is evaluated from it.
	PolicyName string

	// Diff is the downstream's rendered unified diff for a file mutation, when
	// it supplied one, so an operator answering the ask can see what would
	// change. Empty otherwise.
	Diff string

	// MayCall and MayCallDeclared are the DECLARED reach the downstream sent
	// (Meta.MayCall / Meta.MayCallDeclared): the tools the gated call may itself
	// invoke, and how to read that list. They are display provenance for the
	// human answering, exactly like PolicyName — nothing is evaluated from
	// them, and a declaration is not a proof. See Meta.MayCall.
	MayCall         []string
	MayCallDeclared *bool

	// Title is the request's human-facing label (toolCall.title), falling back
	// to the tool-call id. It is always populated with SOMETHING, because an
	// inbox row with no description at all is unanswerable — it is display
	// text and is never matched against a policy.
	Title string

	// ToolCallID is the downstream's own id for the gated call, carried through
	// so an ask can be correlated back to the tool call that raised it.
	ToolCallID string
}

// MapRequest projects req onto hitlservice's evaluator inputs. It never fails:
// a request it cannot name comes back with Named false, which the caller is
// required to treat as "requires approval" (see the file comment).
func MapRequest(req libacp.RequestPermissionRequest) Mapped {
	m := Mapped{
		Args:       map[string]any{},
		Title:      strings.TrimSpace(req.ToolCall.Title),
		ToolCallID: req.ToolCall.ToolCallID,
	}
	if m.Title == "" {
		m.Title = req.ToolCall.ToolCallID
	}

	// The request-level envelope wins over the tool-call one; BuildRequest
	// writes identical bytes to both, so this only matters for a downstream
	// that populated one of them.
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

// ParseMeta decodes the `_meta` envelope MarshalMeta wrote. It reports false
// for absent, malformed, or empty-of-every-field metadata, so a caller can tell
// "the downstream said nothing" from "the downstream said nothing useful"
// without inspecting the zero value itself.
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

// ParseArgs decodes rawInput as the argument map MarshalArgs wrote. It reports
// false when there is nothing to decode or the payload is not a JSON object (an
// array or scalar rawInput is a legal ACP payload, but it is not an argument map
// a policy condition can be evaluated against).
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

// SelectOption picks the option id to answer req with: the first option whose
// kind grants (allow_once, then allow_always) when allow is true, otherwise the
// first that refuses (reject_once, then reject_always). It reports false when
// the downstream offered no option of the required polarity, which the caller
// answers as a graceful "cancelled" instead of inventing an id the downstream
// never offered.
//
// The once-before-always preference is deliberate: an answerer deciding ONE
// request must not silently hand the downstream a standing grant for every
// future one.
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

// Answer builds the response that grants or refuses req. A refusal the
// downstream offered no reject option for degrades to the spec-graceful
// "cancelled" outcome — the same shape an unattended session answers with — so
// the downstream turn ends cleanly rather than faulting on an unknown option id.
// A GRANT with no allow option offered likewise degrades to cancelled: there is
// no id that would mean yes, and cancelled is the only honest thing left to say.
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
