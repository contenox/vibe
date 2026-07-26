package hitlservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// vet.go is the authoring-time validator for hitl-policy (envelope) documents.
//
// It is deliberately STRICTER than loadPolicy: the runtime load path stays lax
// about unknown fields outside "compute" so existing policies keep loading, but
// `contenox vet` exists to teach the author, and an unknown field at authoring
// time is almost always a typo that silently disarms the rule the operator
// thought they wrote (a "timeout" that is not "timeout_s" waits forever; a
// "tool_s" never matches). The one sanctioned escape hatch is the repo's own
// comment convention: any key starting with "//" is an annotation, not config,
// and is accepted everywhere (the shipped presets use "//compute").
//
// All defects are collected and joined so one vet run teaches everything at
// once; the result wraps ErrEnvelopeVet.

// ErrEnvelopeVet marks every defect the envelope vet reports.
var ErrEnvelopeVet = errors.New("hitl policy failed validation")

// maxRuleTimeoutS caps a rule's approval timeout at 7 days. Like the compute
// caps it is defensive, not aesthetic: a timeout past a week is a fat-fingered
// value, not an intent, and it must fail where it is written rather than hold
// an approval goroutine to a date.
const maxRuleTimeoutS = 7 * 24 * 60 * 60

// VetPolicy validates a hitl-policy document for authoring: JSON shape,
// unknown fields, rule shapes, tool patterns, and timeout values — then the
// same semantic validation the runtime load path applies (validatePolicy), so
// vet can never pass a policy the runtime would refuse.
func VetPolicy(data []byte) error {
	var errs []error

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%w: not a JSON object: %v", ErrEnvelopeVet, err)
	}
	errs = append(errs, vetUnknownKeys("", top, []string{"default_action", "rules", "compute", "attention"})...)
	errs = append(errs, vetRuleShapes(top["rules"])...)
	errs = append(errs, vetSubObjectShape("compute", top["compute"], []string{"maxTurns", "maxToolCalls", "maxTokens", "modelAllowlist", "backendAllowlist", "onExhausted"})...)
	errs = append(errs, vetSubObjectShape("attention", top["attention"], []string{"allowAgentAnswers", "maxAgentAnswers"})...)

	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		errs = append(errs, fmt.Errorf("policy does not parse: %v", err))
		return fmt.Errorf("%w:\n%w", ErrEnvelopeVet, errors.Join(errs...))
	}
	// The runtime's own semantic checks: actions, on_timeout, glob validity,
	// compute and attention bounds. Reused, not reimplemented, so vet and the
	// load path can never disagree on semantics.
	if err := validatePolicy(&p); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, vetRuleSemantics(&p)...)

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n%w", ErrEnvelopeVet, errors.Join(errs...))
}

// isCommentKey reports whether a JSON key is an annotation under the repo's
// "//"-prefix convention (e.g. the presets' "//compute" note).
func isCommentKey(k string) bool { return strings.HasPrefix(k, "//") }

func vetUnknownKeys(where string, obj map[string]json.RawMessage, known []string) []error {
	var errs []error
	for k := range obj {
		if isCommentKey(k) {
			continue
		}
		found := false
		for _, kn := range known {
			if k == kn {
				found = true
				break
			}
		}
		if !found {
			prefix := "policy"
			if where != "" {
				prefix = where
			}
			errs = append(errs, fmt.Errorf("%s: unknown field %q — known fields: %s (keys starting with // are treated as comments)",
				prefix, k, strings.Join(known, ", ")))
		}
	}
	return errs
}

// vetRuleShapes strict-checks every rule object and its conditions for unknown
// fields, since Rule is where the silent-typo hazard concentrates.
func vetRuleShapes(rulesRaw json.RawMessage) []error {
	if len(rulesRaw) == 0 {
		return nil
	}
	var rules []map[string]json.RawMessage
	if err := json.Unmarshal(rulesRaw, &rules); err != nil {
		return []error{fmt.Errorf("rules: must be an array of rule objects: %v", err)}
	}
	knownRule := []string{"tools", "tool", "when", "action", "timeout_s", "on_timeout"}
	knownCond := []string{"key", "op", "value"}
	var errs []error
	for i, rule := range rules {
		for _, e := range vetUnknownKeys(fmt.Sprintf("rule %d", i), rule, knownRule) {
			errs = append(errs, e)
		}
		if whenRaw, ok := rule["when"]; ok && len(whenRaw) > 0 {
			var conds []map[string]json.RawMessage
			if err := json.Unmarshal(whenRaw, &conds); err != nil {
				errs = append(errs, fmt.Errorf("rule %d: when must be an array of {key, op, value} conditions: %v", i, err))
				continue
			}
			for j, cond := range conds {
				errs = append(errs, vetUnknownKeys(fmt.Sprintf("rule %d, condition %d", i, j), cond, knownCond)...)
			}
		}
	}
	return errs
}

func vetSubObjectShape(name string, raw json.RawMessage, known []string) []error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return []error{fmt.Errorf("%s: must be an object: %v", name, err)}
	}
	return vetUnknownKeys(name, obj, known)
}

// vetRuleSemantics adds the authoring-time checks the runtime load path does
// not make: tool name patterns that can never match, and timeout values that
// are dead or absurd.
func vetRuleSemantics(p *Policy) []error {
	var errs []error
	for i, r := range p.Rules {
		// ruleMatches compares tools/tool names EXACTLY, with "*" (and empty)
		// as the only wildcard. A pattern like "local_*" is therefore a rule
		// that silently never fires — the deny the operator believes they have.
		for _, pat := range []struct{ field, value string }{{"tools", r.Tools}, {"tool", r.Tool}} {
			if pat.value == "*" || pat.value == "" {
				continue
			}
			if strings.ContainsAny(pat.value, "*?[") {
				errs = append(errs, fmt.Errorf("rule %d: %s %q can never match — tool names are compared exactly, and the only wildcard is %q on its own; name the tool exactly or use %q",
					i, pat.field, pat.value, "*", "*"))
			}
		}
		if r.TimeoutS < 0 {
			errs = append(errs, fmt.Errorf("rule %d: timeout_s must not be negative (got %d); zero means wait indefinitely", i, r.TimeoutS))
		}
		if r.TimeoutS > maxRuleTimeoutS {
			errs = append(errs, fmt.Errorf("rule %d: timeout_s %d is out of range (max %d, seven days) — a longer wait is a typo, not an intent", i, r.TimeoutS, maxRuleTimeoutS))
		}
		// timeout_s / on_timeout only take effect while an approval is pending;
		// on any other action they are dead config the operator likely meant
		// differently (e.g. believing a deny would wait).
		if r.Action != ActionApprove && (r.TimeoutS != 0 || r.OnTimeout != "") {
			errs = append(errs, fmt.Errorf("rule %d: timeout_s/on_timeout only apply when action is %q — this rule's action %q never waits, so they would silently do nothing",
				i, ActionApprove, r.Action))
		}
	}
	return errs
}
