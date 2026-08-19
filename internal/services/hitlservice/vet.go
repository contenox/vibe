package hitlservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrEnvelopeVet marks every defect the envelope vet reports.
var ErrEnvelopeVet = errors.New("hitl policy failed validation")

// MaxRuleTimeoutS bounds Rule.TimeoutS; every producer of a rule checks against
// this one value rather than restating seven days.
const MaxRuleTimeoutS = 7 * 24 * 60 * 60

// VetPolicy validates a hitl-policy document for authoring — JSON shape,
// unknown fields, rule shapes — then applies validatePolicy, so vet can
// never pass a policy the runtime would refuse.
func VetPolicy(data []byte) error {
	var errs []error

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%w: not a JSON object: %v", ErrEnvelopeVet, err)
	}
	errs = append(errs, vetUnknownKeys("", top, []string{"version", "default_action", "rules", "compute", "attention", "trusted_binaries"})...)
	errs = append(errs, vetRuleShapes(top["rules"])...)
	errs = append(errs, vetSubObjectShape("compute", top["compute"], []string{"maxTurns", "maxToolCalls", "maxTokens", "modelAllowlist", "backendAllowlist", "onExhausted"})...)
	errs = append(errs, vetSubObjectShape("attention", top["attention"], []string{"allowAgentAnswers", "maxAgentAnswers", "allowAgentApprovals", "maxAgentApprovals"})...)
	errs = append(errs, vetSubObjectShape("trusted_binaries", top["trusted_binaries"], []string{"dirs", "hashes"})...)

	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		errs = append(errs, fmt.Errorf("policy does not parse: %v", err))
		return fmt.Errorf("%w:\n%w", ErrEnvelopeVet, errors.Join(errs...))
	}
	// Reused, not reimplemented, so vet and the load path can't disagree.
	if err := validatePolicy(&p); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, vetRuleSemantics(&p)...)

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n%w", ErrEnvelopeVet, errors.Join(errs...))
}

func isAnnotationKey(k string) bool { return strings.HasPrefix(k, "//") || k == "$schema" }

func vetUnknownKeys(where string, obj map[string]json.RawMessage, known []string) []error {
	var errs []error
	for k := range obj {
		if isAnnotationKey(k) {
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
			errs = append(errs, fmt.Errorf("%s: unknown field %q — known fields: %s (keys starting with // are comments; $schema is allowed)",
				prefix, k, strings.Join(known, ", ")))
		}
	}
	return errs
}

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

func vetRuleSemantics(p *Policy) []error {
	var errs []error
	for i, r := range p.Rules {
		// ruleMatches compares names exactly, with "*" as the only wildcard.
		for _, pat := range []struct{ field, value string }{{"tools", r.Tools}, {"tool", r.Tool}} {
			if pat.value == "*" || pat.value == "" {
				continue
			}
			if strings.ContainsAny(pat.value, "*?[") {
				errs = append(errs, fmt.Errorf("rule %d: %s %q can never match — tool names are compared exactly, and the only wildcard is %q on its own; name the tool exactly or use %q",
					i, pat.field, pat.value, "*", "*"))
			}
		}
		if r.TimeoutS > MaxRuleTimeoutS {
			errs = append(errs, fmt.Errorf("rule %d: timeout_s %d is out of range (max %d, seven days) — a longer wait is a typo, not an intent", i, r.TimeoutS, MaxRuleTimeoutS))
		}
		// timeout_s/on_timeout only take effect while an approval is pending.
		if r.Action != ActionApprove && (r.TimeoutS != 0 || r.OnTimeout != "") {
			errs = append(errs, fmt.Errorf("rule %d: timeout_s/on_timeout only apply when action is %q — this rule's action %q never waits, so they would silently do nothing",
				i, ActionApprove, r.Action))
		}
	}
	return errs
}
