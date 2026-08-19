package agentdecl

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/services/hitlservice"
)

// EnvelopeSection is the agents.toml table an operator writes policy in.
const EnvelopeSection = "envelopes"

// maxEnvelopeChainDepth bounds an extends chain, so a deep graph is reported
// rather than walked.
const maxEnvelopeChainDepth = 8

// ErrNoEnvelope reports a name the envelopes table does not carry. Separate
// from a malformed envelope so a caller can fall back rather than fail.
var ErrNoEnvelope = errors.New("agentdecl: no such envelope")

// envelopeNamePattern excludes dots: a dot would collide with TOML sub-table
// syntax, so [envelopes.a.b] could not name an envelope.
var envelopeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// EnvelopePolicyFile is the filename an envelope transpiles to. sync.go derives
// a declared agent's policy filename the same way, so the two families share one
// namespace and a collision is an error rather than an overwrite.
func EnvelopePolicyFile(name string) string { return PolicyFileFor(name) }

// PolicyFileFor is the one filename rule both policy families follow.
func PolicyFileFor(stem string) string { return "hitl-policy-" + stem + ".json" }

// EnvelopeName resolves what an operator may write for one envelope — its bare
// name or the filename it transpiles to — to the bare name.
func EnvelopeName(s string) (string, bool) {
	name := strings.TrimSpace(s)
	name = strings.TrimSuffix(strings.TrimPrefix(name, "hitl-policy-"), ".json")
	if !envelopeNamePattern.MatchString(name) {
		return "", false
	}
	return name, true
}

// Axis names. An axis left unset emits no rule at all: it falls through to
// default_action rather than being implicitly "approve".
const (
	AxisFilesRead       = "files.read"
	AxisFilesWrite      = "files.write"
	AxisShell           = "shell"
	AxisNetworkRead     = "network.read"
	AxisNetworkWrite    = "network.write"
	AxisMissionsFire    = "missions.fire"
	AxisMissionsAnswer  = "missions.answer"
	axisSubstitutionOff = "off"
)

// AxisGrant is one grant an operator wrote — a capability axis, a tools
// pattern, or default_action: an action, plus the refinements that grant's
// position accepts. `shell = "approve"` is sugar for
// `shell = { grant = "approve" }`; both forms decode here, so the two can never
// diverge.
type AxisGrant struct {
	Grant string
	// DenyPaths and ApprovePaths refine the two files axes.
	DenyPaths    []string
	ApprovePaths []string
	// Blacklist, Substitution, PrefixAllowlist and AskAlways refine shell.
	Blacklist       []string
	Substitution    string
	PrefixAllowlist []string
	AskAlways       []string
	// DenyHosts refines the two network axes.
	DenyHosts []string
	// Timeout zero emits no rule deadline, leaving the ask on the operator's approval ceiling; a negative Timeout is hitlservice.WaitIndefinite, which emits no deadline at all; OnTimeout "" is its deny.
	Timeout   time.Duration
	OnTimeout string
}

var timeoutKeys = []string{"timeout", "on_timeout"}

func (g AxisGrant) bounded(rule hitlservice.Rule) hitlservice.Rule {
	if rule.Action != hitlservice.ActionApprove {
		return rule
	}
	rule.TimeoutS = int(g.Timeout / time.Second)
	if hitlservice.Indefinite(g.Timeout) {
		rule.TimeoutS = hitlservice.TimeoutIndefinite
	}
	rule.OnTimeout = hitlservice.Action(g.OnTimeout)
	return rule
}

func (g AxisGrant) bounds() bool { return g.Timeout != 0 || g.OnTimeout != "" }

// Envelope is one [envelopes.<name>] section, parsed but not yet layered onto
// its parent. Compute, Attention and TrustedBinaries stay raw so extends can
// merge them per leaf key; a typed struct could not tell an absent bound from a
// zero one.
type Envelope struct {
	Name            string
	Extends         string
	Description     string
	DefaultAction   AxisGrant
	Axes            map[string]AxisGrant
	Tools           map[string]AxisGrant
	Compute         map[string]any
	Attention       map[string]any
	TrustedBinaries map[string]any
	AlwaysDeny      []StandingRule
	AlwaysAllow     []StandingRule
}

// EnvelopeNames lists the declared envelopes, sorted.
func (cfg Config) EnvelopeNames() []string {
	out := make([]string, 0, len(cfg.Envelopes))
	for name := range cfg.Envelopes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ParseEnvelope decodes one declared envelope without resolving its extends.
func (cfg Config) ParseEnvelope(name string) (Envelope, error) {
	raw, ok := cfg.Envelopes[name]
	if !ok {
		return Envelope{}, fmt.Errorf("%w: %q", ErrNoEnvelope, name)
	}
	if !envelopeNamePattern.MatchString(name) {
		return Envelope{}, fmt.Errorf("agentdecl: [%s.%s]: name must match %s (a dot would collide with TOML sub-table syntax)",
			EnvelopeSection, name, envelopeNamePattern)
	}
	return parseEnvelope(name, raw)
}

// ResolveEnvelope returns the envelope with its extends chain applied, parent
// first. A missing parent, a cycle, and a chain deeper than
// maxEnvelopeChainDepth are all errors naming the offender.
func (cfg Config) ResolveEnvelope(name string) (Envelope, error) {
	if name == "" {
		return Envelope{}, fmt.Errorf("%w: the empty name", ErrNoEnvelope)
	}
	var chain []Envelope
	seen := map[string]bool{}
	for cur := name; cur != ""; {
		if seen[cur] {
			return Envelope{}, fmt.Errorf("agentdecl: [%s.%s]: extends forms a cycle through %q", EnvelopeSection, name, cur)
		}
		seen[cur] = true
		env, err := cfg.ParseEnvelope(cur)
		if err != nil {
			if errors.Is(err, ErrNoEnvelope) && cur != name {
				return Envelope{}, fmt.Errorf("agentdecl: [%s.%s]: extends names %q, which no envelope declares", EnvelopeSection, name, cur)
			}
			return Envelope{}, err
		}
		chain = append(chain, env)
		if len(chain) > maxEnvelopeChainDepth {
			return Envelope{}, fmt.Errorf("agentdecl: [%s.%s]: extends chain is deeper than %d envelopes", EnvelopeSection, name, maxEnvelopeChainDepth)
		}
		cur = env.Extends
	}
	out := Envelope{Name: name}
	for i := len(chain) - 1; i >= 0; i-- {
		out = layerEnvelope(out, chain[i])
	}
	out.Name, out.Extends = name, chain[0].Extends
	return out, nil
}

// layerEnvelope applies child onto parent. Scalars and axis grants merge per
// leaf key; a list-valued refinement replaces the parent's wholesale, since
// silent concatenation would make a deny list impossible to shrink. Standing
// rules are the one place accumulation is correct: they exist to be unwaivable.
func layerEnvelope(parent, child Envelope) Envelope {
	out := parent
	if child.Description != "" {
		out.Description = child.Description
	}
	if child.DefaultAction.Grant != "" {
		out.DefaultAction = child.DefaultAction
	}
	if len(child.Axes) > 0 {
		merged := make(map[string]AxisGrant, len(out.Axes)+len(child.Axes))
		for k, v := range out.Axes {
			merged[k] = v
		}
		for k, v := range child.Axes {
			merged[k] = v
		}
		out.Axes = merged
	}
	if len(child.Tools) > 0 {
		merged := make(map[string]AxisGrant, len(out.Tools)+len(child.Tools))
		for k, v := range out.Tools {
			merged[k] = v
		}
		for k, v := range child.Tools {
			merged[k] = v
		}
		out.Tools = merged
	}
	out.Compute = mergeBlock(out.Compute, child.Compute)
	out.Attention = mergeBlock(out.Attention, child.Attention)
	out.TrustedBinaries = mergeBlock(out.TrustedBinaries, child.TrustedBinaries)
	out.AlwaysDeny = concatStandingRules(out.AlwaysDeny, child.AlwaysDeny)
	out.AlwaysAllow = concatStandingRules(out.AlwaysAllow, child.AlwaysAllow)
	return out
}

func concatStandingRules(parent, child []StandingRule) []StandingRule {
	out := make([]StandingRule, 0, len(parent)+len(child))
	seen := map[StandingRule]bool{}
	for _, r := range append(append([]StandingRule{}, parent...), child...) {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseEnvelope(name string, raw map[string]any) (Envelope, error) {
	env := Envelope{Name: name}
	fail := func(format string, args ...any) (Envelope, error) {
		return Envelope{}, fmt.Errorf("agentdecl: [%s.%s]: %s", EnvelopeSection, name, fmt.Sprintf(format, args...))
	}
	for _, key := range sortedKeys(raw) {
		value := raw[key]
		var err error
		switch key {
		case "extends":
			env.Extends, err = envelopeString(key, value)
			if err == nil && !envelopeNamePattern.MatchString(env.Extends) {
				err = fmt.Errorf("extends %q is not a valid envelope name", env.Extends)
			}
		case "description":
			env.Description, err = envelopeString(key, value)
		case "default_action":
			env.DefaultAction, err = parseAxisGrant(key, value)
		case "files":
			err = parseAxisTable(&env, "files", value, map[string]string{"read": AxisFilesRead, "write": AxisFilesWrite})
		case "network":
			err = parseAxisTable(&env, "network", value, map[string]string{"read": AxisNetworkRead, "write": AxisNetworkWrite})
		case "missions":
			err = parseAxisTable(&env, "missions", value, map[string]string{"fire": AxisMissionsFire, "answer": AxisMissionsAnswer})
		case "shell":
			var grant AxisGrant
			grant, err = parseAxisGrant(AxisShell, value)
			if err == nil {
				env.setAxis(AxisShell, grant)
			}
		case "tools":
			env.Tools, err = parseToolPatterns(value)
		case "compute":
			env.Compute, err = envelopeBlock(key, value, computeBlockKeys)
		case "attention":
			env.Attention, err = envelopeBlock(key, value, attentionBlockKeys)
		case "trusted_binaries":
			env.TrustedBinaries, err = envelopeBlock(key, value, trustedBinaryBlockKeys)
		case "always_deny":
			env.AlwaysDeny, err = parseStandingRules(key, value)
		case "always_allow":
			env.AlwaysAllow, err = parseStandingRules(key, value)
		default:
			err = fmt.Errorf("unknown key %q — known keys: %s", key, strings.Join(envelopeKeys, ", "))
		}
		if err != nil {
			return fail("%v", err)
		}
	}
	return env, nil
}

var envelopeKeys = []string{
	"extends", "description", "default_action", "files.read", "files.write", "shell",
	"network.read", "network.write", "missions.fire", "missions.answer",
	"tools", "compute", "attention", "trusted_binaries", "always_deny", "always_allow",
}

var (
	computeBlockKeys       = []string{"max_tool_calls", "max_tokens", "max_turns", "on_exhausted", "model_allowlist", "backend_allowlist"}
	attentionBlockKeys     = []string{"allow_agent_answers", "max_agent_answers", "allow_agent_approvals", "max_agent_approvals"}
	trustedBinaryBlockKeys = []string{"dirs", "hashes"}
)

func (env *Envelope) setAxis(axis string, grant AxisGrant) {
	if env.Axes == nil {
		env.Axes = map[string]AxisGrant{}
	}
	env.Axes[axis] = grant
}

// parseAxisTable decodes the sub-table half of a dotted axis key. Dotted keys
// and sub-tables are the same document, so `files.read = "allow"` and a
// [envelopes.x.files] with `read = "allow"` arrive here identically.
func parseAxisTable(env *Envelope, section string, value any, axes map[string]string) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a table of %s", section, strings.Join(sortedKeys(axes), "/"))
	}
	for _, key := range sortedKeys(table) {
		axis, known := axes[key]
		if !known {
			return fmt.Errorf("unknown key %q under %s — known keys: %s", key, section, strings.Join(sortedKeys(axes), ", "))
		}
		grant, err := parseAxisGrant(axis, table[key])
		if err != nil {
			return err
		}
		env.setAxis(axis, grant)
	}
	return nil
}

// parseAxisGrant decodes both grant forms: a bare action string, and the inline
// table carrying `grant` plus that position's refinements.
func parseAxisGrant(axis string, value any) (AxisGrant, error) {
	switch v := value.(type) {
	case string:
		action, err := envelopeAction(axis, v)
		if err != nil {
			return AxisGrant{}, err
		}
		return AxisGrant{Grant: action}, nil
	case map[string]any:
		return parseAxisGrantTable(axis, v)
	default:
		return AxisGrant{}, fmt.Errorf("%s must be an action string or a table carrying grant, got %T", axis, value)
	}
}

func parseAxisGrantTable(axis string, table map[string]any) (AxisGrant, error) {
	grant := AxisGrant{}
	refinements := axisRefinements(axis)
	if _, ok := table["grant"]; !ok {
		return AxisGrant{}, fmt.Errorf("%s: the table form requires grant", axis)
	}
	for _, key := range sortedKeys(table) {
		value := table[key]
		var err error
		switch {
		case key == "grant":
			grant.Grant, err = envelopeAction(axis+".grant", value)
		case key == "timeout":
			grant.Timeout, err = envelopeTimeout(axis, value)
		case key == "on_timeout":
			grant.OnTimeout, err = envelopeOnTimeout(axis, value)
		case !refinements[key]:
			known := append(append([]string{"grant"}, timeoutKeys...), sortedKeys(refinements)...)
			err = fmt.Errorf("unknown key %q under %s — known keys: %s", key, axis, strings.Join(known, ", "))
		case key == "substitution":
			var s string
			s, err = envelopeString(axis+".substitution", value)
			if err == nil {
				switch s {
				case string(hitlservice.ActionDeny), string(hitlservice.ActionApprove), axisSubstitutionOff:
					grant.Substitution = s
				default:
					err = fmt.Errorf("%s.substitution is %q (want deny, approve or off)", axis, s)
				}
			}
		default:
			var list []string
			// A path list emits one rule per glob, so a brace expression's commas
			// are meaningful there; every other list is joined into one value.
			list, err = envelopeStringList(axis+"."+key, value, key != "deny_paths" && key != "approve_paths")
			if err == nil {
				switch key {
				case "deny_paths":
					grant.DenyPaths = list
				case "approve_paths":
					grant.ApprovePaths = list
				case "blacklist":
					grant.Blacklist = list
				case "prefix_allowlist":
					grant.PrefixAllowlist = list
				case "ask_always":
					grant.AskAlways = list
				case "deny_hosts":
					grant.DenyHosts = list
				}
			}
		}
		if err != nil {
			return AxisGrant{}, err
		}
	}
	if err := checkGrantWait(axis, grant); err != nil {
		return AxisGrant{}, err
	}
	return grant, nil
}

func checkGrantWait(axis string, grant AxisGrant) error {
	if !grant.bounds() {
		return nil
	}
	switch axis {
	case AxisMissionsAnswer:
		return fmt.Errorf("%s: timeout/on_timeout do not apply here — this axis compiles to the attention block, which carries no wait, not to a rule", axis)
	case AxisNetworkRead, AxisNetworkWrite:
		return fmt.Errorf("%s: timeout/on_timeout do not apply here — no provider serves this axis in this build, so it emits no rule to bound", axis)
	}
	if !grantEmitsAsk(axis, grant) {
		return fmt.Errorf("%s: timeout/on_timeout apply to an ask, and grant = %q never asks — only %q waits for a human",
			axis, grant.Grant, hitlservice.ActionApprove)
	}
	if hitlservice.Indefinite(grant.Timeout) && grant.OnTimeout != "" {
		return fmt.Errorf("%s: on_timeout = %q cannot apply to timeout = %q — an ask with no deadline never expires, so nothing would ever read on_timeout; drop one of the two",
			axis, grant.OnTimeout, hitlservice.FormatWait(grant.Timeout))
	}
	return nil
}

func grantEmitsAsk(axis string, grant AxisGrant) bool {
	if grant.Grant == string(hitlservice.ActionApprove) {
		return true
	}
	switch axis {
	case AxisFilesRead, AxisFilesWrite:
		return len(grant.ApprovePaths) > 0
	case AxisShell:
		return len(grant.AskAlways) > 0 || grant.Substitution == string(hitlservice.ActionApprove)
	}
	return false
}

func axisRefinements(axis string) map[string]bool {
	switch axis {
	case AxisFilesRead, AxisFilesWrite:
		return map[string]bool{"deny_paths": true, "approve_paths": true}
	case AxisShell:
		return map[string]bool{"blacklist": true, "substitution": true, "prefix_allowlist": true, "ask_always": true}
	case AxisNetworkRead, AxisNetworkWrite:
		return map[string]bool{"deny_hosts": true}
	default:
		return map[string]bool{}
	}
}

func envelopeString(key string, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", key, value)
	}
	return strings.TrimSpace(s), nil
}

func envelopeAction(key string, value any) (string, error) {
	s, err := envelopeString(key, value)
	if err != nil {
		return "", err
	}
	if _, err := parseAction(s); err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return s, nil
}

func envelopeTimeout(axis string, value any) (time.Duration, error) {
	s, err := envelopeString(axis+".timeout", value)
	if err != nil {
		return 0, err
	}
	if hitlservice.IsIndefiniteWord(s) {
		return hitlservice.WaitIndefinite, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s.timeout is %q, which is not a duration — write it as Go writes one: 90s, 30m, 2h, or one of %s for an ask that waits until it is answered",
			axis, s, hitlservice.IndefiniteSpellings())
	}
	switch {
	case d < 0:
		return 0, fmt.Errorf("%s.timeout is %q; a wait cannot run backwards", axis, s)
	case d == 0:
		return 0, fmt.Errorf("%s.timeout is %q, which states no wait at all; omit timeout to leave the rule without a deadline", axis, s)
	case d%time.Second != 0:
		return 0, fmt.Errorf("%s.timeout is %q; the policy carries whole seconds, so this would be truncated to %s — write the wait you mean", axis, s, d.Truncate(time.Second))
	case int64(d/time.Second) > hitlservice.MaxRuleTimeoutS:
		return 0, fmt.Errorf("%s.timeout is %q, longer than the %s a rule accepts — write one of %s for an ask that waits until it is answered, rather than a number that means it",
			axis, s, time.Duration(hitlservice.MaxRuleTimeoutS)*time.Second, hitlservice.IndefiniteSpellings())
	}
	return d, nil
}

func envelopeOnTimeout(axis string, value any) (string, error) {
	s, err := envelopeString(axis+".on_timeout", value)
	if err != nil {
		return "", err
	}
	action, err := parseAction(s)
	if err != nil {
		return "", fmt.Errorf("%s.on_timeout: %w", axis, err)
	}
	switch action {
	case hitlservice.ActionDeny:
		return string(action), nil
	case hitlservice.ActionApprove:
		return "", fmt.Errorf("%s.on_timeout is %q, which decides nothing: the wait exists because a human had to approve, and the runtime resolves every expiry that is not %q as a denial anyway — write %q",
			axis, s, hitlservice.ActionAllow, hitlservice.ActionDeny)
	default:
		return "", fmt.Errorf("%s.on_timeout is %q, which the policy schema refuses: an ask that allows itself when nobody answers bypasses the approval it exists to require — write %q",
			axis, s, hitlservice.ActionDeny)
	}
}

// envelopeStringList decodes a refinement list. joined marks the lists that
// become one comma-separated Condition.Value, where an element carrying a comma
// is refused rather than emitted as two.
func envelopeStringList(key string, value any, joined bool) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %T", key, value)
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string, got %T", key, i, item)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("%s[%d] is empty", key, i)
		}
		if joined && strings.Contains(s, ",") {
			return nil, fmt.Errorf("%s[%d] %q contains a comma; the engine reads this list as one comma-separated value, so the entry would be read as two", key, i, s)
		}
		out = append(out, s)
	}
	return out, nil
}

func envelopeBlock(key string, value any, known []string) (map[string]any, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a table, got %T", key, value)
	}
	for _, k := range sortedKeys(table) {
		found := false
		for _, kn := range known {
			if k == kn {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown key %q under %s — known keys: %s", k, key, strings.Join(known, ", "))
		}
	}
	return table, nil
}

func parseStandingRules(key string, value any) ([]StandingRule, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of tables, got %T", key, value)
	}
	known := []string{"tools", "tool", "when_key", "when_op", "when_value"}
	out := make([]StandingRule, 0, len(items))
	for i, item := range items {
		table, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a table, got %T", key, i, item)
		}
		var rule StandingRule
		for _, k := range sortedKeys(table) {
			s, err := envelopeString(fmt.Sprintf("%s[%d].%s", key, i, k), table[k])
			if err != nil {
				return nil, err
			}
			switch k {
			case "tools":
				rule.Tools = s
			case "tool":
				rule.Tool = s
			case "when_key":
				rule.WhenKey = s
			case "when_op":
				rule.WhenOp = s
			case "when_value":
				rule.WhenValue = s
			default:
				return nil, fmt.Errorf("unknown key %q under %s[%d] — known keys: %s", k, key, i, strings.Join(known, ", "))
			}
		}
		out = append(out, rule)
	}
	return out, nil
}

func mergeBlock(parent, child map[string]any) map[string]any {
	if len(child) == 0 {
		return parent
	}
	if len(parent) == 0 {
		return child
	}
	return mergeRaw(parent, child)
}

// toolPatternName is one half of a tool pattern: a literal name, never a
// partial glob, because ruleMatches compares exactly and treats only "" and "*"
// as wildcards.
var toolPatternName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// parseToolPatterns decodes [envelopes.<name>.tools]: pattern -> grant, where
// pattern := "*" | <toolset> | <toolset> "." <tool> and each half is a literal
// name or "*".
func parseToolPatterns(value any) (map[string]AxisGrant, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tools must be a table of pattern = action, or pattern = a table carrying grant, got %T", value)
	}
	out := make(map[string]AxisGrant, len(table))
	for _, pattern := range sortedKeys(table) {
		if _, _, err := ToolPatternRef(pattern); err != nil {
			return nil, err
		}
		grant, err := parseAxisGrant("tools."+pattern, table[pattern])
		if err != nil {
			return nil, err
		}
		out[pattern] = grant
	}
	return out, nil
}

// ToolPatternRef maps one tool pattern onto the (tools, tool) pair a rule
// carries. A bare toolset is that toolset's wildcard.
func ToolPatternRef(pattern string) (tools, tool string, err error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return "", "", fmt.Errorf("tools: an empty pattern matches nothing; write %q for every tool", "*")
	}
	if p == "*" {
		return "*", "*", nil
	}
	parts := strings.Split(p, ".")
	if len(parts) > 2 {
		return "", "", fmt.Errorf("tools: pattern %q has more than one dot; a pattern is %q, %q or %q",
			pattern, "*", "<toolset>", "<toolset>.<tool>")
	}
	for _, part := range parts {
		if part == "*" || toolPatternName.MatchString(part) {
			continue
		}
		return "", "", fmt.Errorf("tools: %q can never match — a name is compared exactly and the only wildcard is %q on its own, so a partial glob like %q is not a pattern",
			pattern, "*", part)
	}
	if len(parts) == 1 {
		return parts[0], "*", nil
	}
	return parts[0], parts[1], nil
}

// toolPatternSpecificity orders the tools table: exact toolset.tool first, then
// one wildcard, then "*". Order in the file is not precedence.
func toolPatternSpecificity(tools, tool string) int {
	n := 0
	if tools == "*" {
		n++
	}
	if tool == "*" {
		n++
	}
	return n
}
