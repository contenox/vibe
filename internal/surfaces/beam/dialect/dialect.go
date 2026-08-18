package dialect

import (
	"encoding/json"
	"strings"

	libacp "github.com/contenox/contenox/libacp"
)

const (
	OptionAllow = "allow"
	OptionDeny  = "deny"
)

const (
	TerminalOutputMetaKey = "contenox.terminalOutput"

	TerminalOutputUpdateKind libacp.SessionUpdateKind = "_contenox.terminalOutput"
)

const (
	configIDModel      = "model"
	configIDHITLPolicy = "hitl-policy"
	configIDThink      = "think"

	hitlPolicyDefaultValue  = "__contenox_default__"
	modelConfigDefaultGroup = "default"
)

type Meta struct {
	ToolsName  string `json:"toolsName,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	PolicyName string `json:"policyName,omitempty"`
	PolicyPath string `json:"policyPath,omitempty"`
	Diff       string `json:"diff,omitempty"`
	DiffOld    string `json:"diffOld,omitempty"`
	DiffNew    string `json:"diffNew,omitempty"`

	// MatchedRule is the 0-based index, in the active policy's rule list, of
	// the rule that gated this call; nil when no rule matched and the
	// policy's DefaultAction applied instead. Mirrors
	// hitlservice.EvaluationResult.MatchedRule onto the wire.
	MatchedRule *int `json:"matchedRule,omitempty"`

	// Detail is the matched rule's human-readable cause -- what in the call
	// actually tripped it (e.g. which shell command), when the rule has one.
	// Mirrors hitlservice.EvaluationResult.Detail onto the wire. Empty for
	// rules with no such cause, or when DefaultAction applied.
	Detail string `json:"detail,omitempty"`

	// MayCall is the gated call's declared reach, in order; a declaration, not a proof.
	MayCall []string `json:"mayCall,omitempty"`

	// MayCallDeclared: nil = unknown; true = MayCall exhaustive; false = reaches others, undeclared.
	MayCallDeclared *bool `json:"mayCallDeclared,omitempty"`
}

func (m Meta) IsZero() bool {
	return m.ToolsName == "" &&
		m.ToolName == "" &&
		m.PolicyName == "" &&
		m.PolicyPath == "" &&
		m.Diff == "" &&
		m.DiffOld == "" &&
		m.DiffNew == "" &&
		m.MatchedRule == nil &&
		m.Detail == "" &&
		len(m.MayCall) == 0 &&
		m.MayCallDeclared == nil
}

func ParseMeta(raw json.RawMessage) (Meta, bool) {
	if len(raw) == 0 {
		return Meta{}, false
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meta{}, false
	}
	if m.IsZero() {
		return Meta{}, false
	}
	return m, true
}

func SummarizeToolCallArgs(toolName string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	base := toolName
	if idx := strings.LastIndex(base, "."); idx >= 0 {
		base = base[idx+1:]
	}
	asString := func(key string) string {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	asStringSlice := func(key string) []string {
		v, ok := args[key]
		if !ok {
			return nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}

	var summary string
	switch base {
	case "exec", "run", "execute", "local_shell":
		cmd := asString("command")
		if cmd == "" {
			break
		}
		parts := []string{cmd}
		if a := asString("args"); a != "" {
			parts = append(parts, a)
		} else {
			parts = append(parts, asStringSlice("args")...)
		}
		summary = strings.Join(parts, " ")
	case "read_file", "read_file_range", "write_file", "stat_file", "list_dir", "delete_file":
		summary = asString("path")
	case "grep":
		if p := asString("pattern"); p != "" {
			if path := asString("path"); path != "" {
				summary = p + " in " + path
			} else {
				summary = p
			}
		}
	case "sed":
		if path := asString("path"); path != "" {
			if pat := asString("pattern"); pat != "" {
				summary = pat + " in " + path
			} else {
				summary = path
			}
		}
	case "fetch_url", "fetch", "http_get":
		summary = asString("url")
	}
	if summary == "" {
		for _, key := range []string{"path", "command", "url", "pattern"} {
			if value := asString(key); strings.TrimSpace(value) != "" {
				summary = value
				break
			}
		}
	}
	if summary == "" {
		return ""
	}
	summary = strings.TrimSpace(strings.ReplaceAll(summary, "\n", " "))
	const maxRunes = 80
	if r := []rune(summary); len(r) > maxRunes {
		summary = string(r[:maxRunes-3]) + "..."
	}
	return summary
}

// Command names whose single argument has a value domain — the keys
// CommandValueDomains returns, matching the wire names from allACPCommands.
const (
	CommandModel    = "model"
	CommandProvider = "provider"
	CommandThink    = "think"
	CommandPolicy   = "policy"
)

// CommandValueDomains projects a client's already-handed session config
// options onto the argument domains of /model, /provider, /think, /policy —
// a completion aid, not a gate; an absent key means "anything is fine".
// /model strips the select's "provider/model" group prefix; /provider uses
// the select's groups (already advertise-what-works filtered); /think is the
// think select verbatim; /policy is the HITL select minus its
// use-the-default sentinel. Wire order and first-seen dedup are preserved.
func CommandValueDomains(options []libacp.SessionConfigOption) map[string][]string {
	out := map[string][]string{}
	for _, option := range options {
		switch option.ID {
		case configIDModel:
			models, providers := modelCommandDomains(option)
			addCommandValues(out, CommandModel, models...)
			addCommandValues(out, CommandProvider, providers...)
		case configIDThink:
			for _, value := range option.Options.AllValues() {
				addCommandValues(out, CommandThink, value.Value)
			}
		case configIDHITLPolicy:
			for _, value := range option.Options.AllValues() {
				if value.Value == hitlPolicyDefaultValue {
					continue
				}
				addCommandValues(out, CommandPolicy, value.Value)
			}
		}
	}
	return out
}

func modelCommandDomains(option libacp.SessionConfigOption) (models, providers []string) {
	for _, group := range option.Options.Groups {
		provider := strings.TrimSpace(group.Group)
		if provider != "" && provider != modelConfigDefaultGroup {
			providers = append(providers, provider)
		}
		for _, value := range group.Options {
			models = append(models, modelFromConfigValue(value.Value, provider))
		}
	}
	// An external session may forward an ungrouped select verbatim; fall
	// back to the wire's own encoding.
	for _, value := range option.Options.Values {
		provider, model := splitModelConfigValue(value.Value)
		if provider != "" {
			providers = append(providers, provider)
		}
		models = append(models, model)
	}
	return models, providers
}

// modelFromConfigValue recovers the bare model name from a grouped select
// value. The group is the provider modelConfigValue prefixed, so the prefix is
// stripped exactly once and only when it is really there.
func modelFromConfigValue(value, provider string) string {
	value = strings.TrimSpace(value)
	if provider == "" || provider == modelConfigDefaultGroup {
		return value
	}
	return strings.TrimPrefix(value, provider+"/")
}

// addCommandValues appends non-empty, not-yet-seen values to a command's
// domain, preserving wire order.
func addCommandValues(domains map[string][]string, command string, values ...string) {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		existing := domains[command]
		duplicate := false
		for _, seen := range existing {
			if seen == value {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		domains[command] = append(existing, value)
	}
}

func splitModelConfigValue(value string) (provider, model string) {
	value = strings.TrimSpace(value)
	if before, after, ok := strings.Cut(value, "/"); ok {
		return strings.TrimSpace(before), strings.TrimSpace(after)
	}
	return "", value
}
