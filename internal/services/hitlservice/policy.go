package hitlservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

// Action is the outcome of policy evaluation for a tool call.
type Action string

const (
	// ActionAllow passes the tool call through without any approval step.
	ActionAllow Action = "allow"
	// ActionApprove blocks execution and requests human approval before proceeding.
	ActionApprove Action = "approve"
	// ActionDeny rejects the tool call immediately with a soft message to the LLM.
	ActionDeny Action = "deny"
)

// ApprovalRequest describes a tool invocation that requires human review.
// Diff is populated for file-mutation tools (write_file, sed) to show the
// unified diff of what would change.
type ApprovalRequest struct {
	ToolCallID string
	ToolsName  string
	ToolName   string
	Args       map[string]any
	Diff       string
	DiffOld    string
	DiffNew    string

	// PolicyName, MatchedRule, TimeoutS, and OnTimeout carry the policy
	// verdict (hitlservice.EvaluationResult) that produced this ask.
	// HITLWrapper.Exec populates them from the same Evaluate() call it
	// already makes; RequestApproval persists them onto the durable row so an
	// operator can always name which rule gated an action, and so it knows
	// how long to wait and what to do if nobody answers in time. Zero values
	// are safe: TimeoutS<=0 means "the matched rule set no timeout of its
	// own" and OnTimeout=="" means "default deny". The attached-session path
	// (acpsvc.Transport.AskApproval) ignores these fields.
	PolicyName  string
	MatchedRule *int
	TimeoutS    int
	OnTimeout   Action

	// InstanceID, SessionID, AgentName and MissionID attribute the ask to the
	// fleet unit that raised it, and are persisted onto the durable row (see
	// runtimetypes.HITLApproval's "Attribution" section for why an inbox needs
	// them). All four are OPTIONAL: an ask raised by a native chain turn with
	// no unit behind it leaves them empty, and MissionID is empty for an
	// unattended session that is not on a mission. They are supplied by the
	// unattended-permission answerer (fleetservice), which is the only caller
	// that HAS this identity; the attached-session path ignores them.
	InstanceID string
	SessionID  string
	AgentName  string
	MissionID  string
}

// ConditionOp is the comparison operator for a rule condition.
type ConditionOp string

const (
	// OpEq requires the argument value to equal the condition value exactly.
	OpEq ConditionOp = "eq"
	// OpGlob matches the argument value against a glob pattern.
	// Both value and pattern are normalized with path.Clean before matching,
	// preventing path-traversal bypass (e.g. ./src/../etc/passwd → etc/passwd).
	// Supports * (within a path component), ? (single char), and ** (across separators).
	OpGlob ConditionOp = "glob"
	// OpHost treats the argument value as a URL and matches its host against a
	// comma-separated list of host patterns in Value. It is the policy-native
	// way to express SSRF-style host denial (e.g. webtools to localhost or a
	// cloud-metadata endpoint): the host is parsed out of the URL, so a trailing
	// :port or path cannot evade the match the way a raw URL glob can. IP
	// literals match exactly; bare names match the host and any subdomain
	// (api.example.com matches example.com).
	OpHost ConditionOp = "host"
	// OpCommandBlacklist matches the command basename against a comma-separated
	// list of denied commands. Used to block dangerous commands like rm, sudo, dd.
	OpCommandBlacklist ConditionOp = "command_blacklist"
	// OpCommandAskAlways matches the command basename against a comma-separated
	// list of commands that should always require human approval (e.g., rm, sudo, chmod).
	// Unlike OpCommandBlacklist which denies, this operator with action:"approve" creates
	// a HITL pause point for safety-critical commands.
	OpCommandAskAlways ConditionOp = "command_ask_always"
	// OpNoCommandSubstitution blocks commands containing shell substitution
	// patterns ($(), backticks, <(), >()) that could indicate command injection.
	OpNoCommandSubstitution ConditionOp = "no_command_substitution"
)

// Condition is a single key/op/value predicate applied to the args of a tool call.
type Condition struct {
	Key   string      `json:"key"`
	Op    ConditionOp `json:"op"`
	Value string      `json:"value"`
}

// Rule matches a tools+tool pair (with optional conditions) and assigns an action.
// When contains zero conditions the name match alone is sufficient.
// All conditions in When must hold for the rule to match (AND semantics).
type Rule struct {
	Tools  string      `json:"tools"`
	Tool   string      `json:"tool"`
	When   []Condition `json:"when,omitempty"`
	Action Action      `json:"action"`
	// TimeoutS is the number of seconds to wait for a human response when Action is
	// ActionApprove. Zero means no timeout (block indefinitely until ctx is cancelled).
	TimeoutS int `json:"timeout_s,omitempty"`
	// OnTimeout is the fallback action when the approval window expires.
	// Only "deny" and "approve" are valid (allow would silently bypass approval).
	OnTimeout Action `json:"on_timeout,omitempty"`
}

// Policy is the top-level document stored as hitl-policy.json in the VFS.
// Rules are evaluated in order; the first matching rule wins.
// DefaultAction is applied when no rule matches; it is fail-closed to "approve"
// when absent so an unaccounted-for tool pauses for a human.
//
// Compute is the OPTIONAL compute half of the envelope (see ComputeBounds). A nil
// Compute is the default and means UNBOUNDED — an envelope with no compute block
// bounds only ACTIONS (the rules above), exactly as it did before this field
// existed. It is a pointer so its absence is a first-class, wire-visible fact: a
// policy that never grew a compute block round-trips through JSON byte-for-byte as
// before, and the enforcement seams read `Compute == nil` as "this mission is
// bounded only by its rules".
type Policy struct {
	DefaultAction Action         `json:"default_action,omitempty"`
	Rules         []Rule         `json:"rules"`
	Compute       *ComputeBounds `json:"compute,omitempty"`
	// Attention is the OPTIONAL attention half of the envelope: who may answer a
	// unit's question (see AttentionBounds). Nil — the default — means a HUMAN
	// must, which is what the escalation was for.
	Attention *AttentionBounds `json:"attention,omitempty"`
}

// OnExhausted names what a mission does when it crosses one of its envelope's
// compute bounds. It is a small, closed set — the terminal-vs-pause choice the
// operator declares up front, the compute analogue of Rule.OnTimeout.
type OnExhausted string

const (
	// OnExhaustedFinishStuck finishes the mission at StatusStuck through the
	// runtime's real terminal machinery, with a reason naming the bound. It is the
	// default when OnExhausted is unset, and the only behavior enforced today.
	OnExhaustedFinishStuck OnExhausted = "finish_stuck"
	// OnExhaustedPauseAsk is DECLARED but not yet enforced: it will file a durable
	// ask ("this mission hit its compute bound — extend it or let it stop?") instead
	// of finishing. Until that machinery lands, an envelope that sets it is honored
	// AS finish_stuck at the enforcement seam. The validator accepts it so a
	// forward-looking envelope parses today; the blueprint records the deferral.
	OnExhaustedPauseAsk OnExhausted = "pause_ask"
)

// ComputeBounds is the envelope's COMPUTE half: the ceiling a mission's TOTAL
// compute is held under, alongside the per-tool ACTION rules above. It is the
// constitutional widening of the envelope from "what a unit may DO" to "how much a
// unit may SPEND" — the same envelope, now the unit's total boundary. These are
// GATES by design, the legitimate kind: envelope-declared, operator-authored, and
// deterministic at the boundary (turn start, tool dispatch) — not the advice the
// attention layer appends.
//
// Every bound is a CEILING and OPT-IN: a zero/absent field is unbounded, so an
// envelope with no compute block (or an empty one) runs exactly as it did before —
// bounds only ever RESTRICT, never grant. Bounds are per MISSION, checked at
// deterministic seams, and exhaustion is never silent (see OnExhausted).
//
// What is measured and what is NOT yet enforced (honest scope, per the blueprint):
//   - MaxTurns and MaxToolCalls are countable deterministically host-side and ARE
//     enforced — the drive loop's prompt turns, and the unattended answerer's
//     envelope-gated tool dispatches.
//   - MaxTokens is BEST-EFFORT: it is enforced only from the usage the downstream
//     unit actually reports (ACP usage_update), which not every provider emits — it
//     bounds a mission whose unit reports usage and is inert for one whose unit does
//     not, rather than guessing.
//   - ModelAllowlist / BackendAllowlist are DECLARED here and validated for shape,
//     but model/backend resolution happens inside the unit's own process where the
//     host cannot see it, so they are NOT yet enforced. They are parsed so an
//     envelope can express the intent and a later llmrepo-side seam can honor it.
type ComputeBounds struct {
	MaxTurns         int         `json:"maxTurns,omitempty"`
	MaxToolCalls     int         `json:"maxToolCalls,omitempty"`
	MaxTokens        int         `json:"maxTokens,omitempty"`
	ModelAllowlist   []string    `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string    `json:"backendAllowlist,omitempty"`
	OnExhausted      OnExhausted `json:"onExhausted,omitempty"`
}

// Compute-bound validation caps. They are DEFENSIVE, not aesthetic (the register
// of the plan/handover caps in missionservice): they exist to reject a negative or
// an absurd value a hand-edited or hallucinated policy might carry, not to impose a
// house style on how tight a bound should be. A real operator ceiling sits far
// below these; a value past them is a typo (an extra digit, a sign flip) that must
// fail the policy to load rather than silently bound a mission at ten billion.
const (
	maxComputeTurns               = 100_000
	maxComputeToolCalls           = 10_000_000
	maxComputeTokens              = 100_000_000_000
	maxComputeAllowlist           = 256
	maxComputeAllowlistEntryBytes = 512
)

// Reason constants used in EvaluationResult.Reason.
const (
	ReasonMatchedRule   = "matched_rule"
	ReasonDefaultAction = "default_action"
)

// EvaluationResult carries the policy decision plus introspection data.
type EvaluationResult struct {
	Action      Action
	MatchedRule *int   // nil when DefaultAction was applied (no rule matched)
	Reason      string // ReasonMatchedRule or ReasonDefaultAction
	TimeoutS    int
	OnTimeout   Action
	PolicyName  string
}

// evaluate returns the EvaluationResult for the given tools, tool name, and call args.
func evaluate(p *Policy, toolsName, toolName string, args map[string]any) EvaluationResult {
	for i, r := range p.Rules {
		if ruleMatches(r, toolsName, toolName, args) {
			idx := i
			return EvaluationResult{
				Action:      r.Action,
				MatchedRule: &idx,
				Reason:      ReasonMatchedRule,
				TimeoutS:    r.TimeoutS,
				OnTimeout:   r.OnTimeout,
			}
		}
	}
	defaultAction := p.DefaultAction
	if defaultAction == "" {
		defaultAction = ActionApprove
	}
	return EvaluationResult{
		Action: defaultAction,
		Reason: ReasonDefaultAction,
	}
}

func ruleMatches(r Rule, toolsName, toolName string, args map[string]any) bool {
	toolsOK := r.Tools == "" || r.Tools == "*" || r.Tools == toolsName
	toolOK := r.Tool == "" || r.Tool == "*" || r.Tool == toolName
	if !toolsOK || !toolOK {
		return false
	}
	for _, c := range r.When {
		if !conditionMatches(c, args) {
			return false
		}
	}
	return true
}

func conditionMatches(c Condition, args map[string]any) bool {
	val, ok := args[c.Key]
	if !ok {
		return false
	}
	for _, s := range conditionValues(val) {
		switch c.Op {
		case OpEq:
			if s == c.Value {
				return true
			}
		case OpGlob:
			if globMatch(c.Value, s) {
				return true
			}
		case OpHost:
			if urlHostMatches(s, c.Value) {
				return true
			}
		case OpCommandBlacklist:
			if isCommandBlacklisted(args, c.Value) {
				return true
			}
		case OpCommandAskAlways:
			if isCommandInAskAlwaysList(args, c.Value) {
				return true
			}
		case OpNoCommandSubstitution:
			if detectCommandSubstitution(args) {
				return true
			}
		}
	}
	return false
}

// urlHostMatches parses rawURL and reports whether its host equals, or is a
// subdomain of, any comma-separated pattern in patternsCSV. Parsing the host
// out of the URL (rather than substring-matching the raw URL) is what makes a
// host deny rule robust against :port and path evasion. IP literals match
// exactly; bare names match the host and any subdomain.
func urlHostMatches(rawURL, patternsCSV string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	isIP := net.ParseIP(host) != nil
	for _, p := range strings.Split(patternsCSV, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if host == p {
			return true
		}
		if !isIP && strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

// conditionValues flattens an argument value into the strings a condition is
// tested against. A slice argument (e.g. {"path": ["a","/etc/passwd"]}) is
// tested element-wise so a single stringified "[a /etc/passwd]" cannot slip a
// restricted entry past a glob.
func conditionValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprintf("%v", e))
		}
		return out
	default:
		return []string{fmt.Sprintf("%v", v)}
	}
}

// globMatch reports whether s matches the glob pattern.
// Both pattern and s are normalized with path.Clean before comparison to prevent
// path-traversal bypasses. Supports *, ?, and ** (which matches across path separators).
func globMatch(pattern, s string) bool {
	s = path.Clean(s)
	for _, expanded := range expandGlobBraces(pattern) {
		if globMatchOne(expanded, s) {
			return true
		}
	}
	return false
}

func globMatchOne(pattern, s string) bool {
	pattern = path.Clean(pattern)
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == s
	}
	if !strings.Contains(pattern, "**") {
		matched, err := path.Match(pattern, s)
		return err == nil && matched
	}
	return matchDoubleGlob(pattern, s)
}

// expandGlobBraces expands {a,b,c} alternations into the cross product of
// concrete patterns, supporting nesting and slashes inside an alternative.
// Go's path.Match has no brace support, so "**/{.ssh,.gnupg}/**" would
// otherwise match a literal "{" and silently never fire. Unbalanced braces
// are treated literally. Expansion is bounded; past the cap the original
// pattern is returned unexpanded.
const maxGlobExpansions = 4096

func expandGlobBraces(pattern string) []string {
	const maxExpansions = maxGlobExpansions
	out := []string{pattern}
	for {
		next := make([]string, 0, len(out))
		changed := false
		for _, p := range out {
			open, closeIdx, ok := firstBracePair(p)
			if !ok {
				next = append(next, p)
				continue
			}
			changed = true
			prefix, suffix := p[:open], p[closeIdx+1:]
			for _, alt := range splitTopLevelCommas(p[open+1 : closeIdx]) {
				next = append(next, prefix+alt+suffix)
			}
		}
		if !changed {
			return next
		}
		if len(next) > maxExpansions {
			return next[:maxExpansions]
		}
		out = next
	}
}

func firstBracePair(p string) (open, closeIdx int, ok bool) {
	for i := 0; i < len(p); i++ {
		if p[i] != '{' {
			continue
		}
		if c, found := matchingBrace(p, i); found {
			return i, c, true
		}
	}
	return 0, 0, false
}

func matchingBrace(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return -1, false
}

func splitTopLevelCommas(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// matchDoubleGlob handles patterns that contain **.
// ** matches zero or more path components.
func matchDoubleGlob(pattern, s string) bool {
	idx := strings.Index(pattern, "**")
	prefix := strings.TrimSuffix(pattern[:idx], "/")
	after := strings.TrimPrefix(pattern[idx+2:], "/")

	if prefix != "" {
		if s == prefix {
			return after == ""
		}
		if !strings.HasPrefix(s, prefix+"/") {
			return false
		}
		s = s[len(prefix)+1:]
	}

	if after == "" {
		return true
	}

	// Try matching `after` against every path suffix of s (split at each /).
	for {
		if matchSuffix(after, s) {
			return true
		}
		slash := strings.Index(s, "/")
		if slash < 0 {
			break
		}
		s = s[slash+1:]
	}
	return false
}

func matchSuffix(pattern, s string) bool {
	if !strings.Contains(pattern, "**") {
		matched, err := path.Match(pattern, s)
		return err == nil && matched
	}
	return matchDoubleGlob(pattern, s)
}

// commandSubstitutionPatterns are shell metacharacters that enable command
// substitution and should be blocked to prevent injection attacks.
var commandSubstitutionPatterns = []string{
	"$(",  // Command substitution
	"`",   // Backtick command substitution
	"<(",  // Process substitution (read)
	">(",  // Process substitution (write)
	"$[",  // Arithmetic expansion
	"${}", // Parameter expansion
	"$((", // Arithmetic expansion
}

// detectCommandSubstitution checks if a command string or any of its arguments
// contain shell metacharacters that could enable command injection.
func detectCommandSubstitution(args map[string]any) bool {
	// Check all argument values for substitution patterns
	for _, v := range args {
		for _, s := range conditionValues(v) {
			for _, pattern := range commandSubstitutionPatterns {
				if strings.Contains(s, pattern) {
					return true
				}
			}
		}
	}
	return false
}

// getCommandFromArgs extracts the command string from tool arguments.
// For local_shell, this checks both "command" and the first element of "args".
func getCommandFromArgs(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok {
		return cmd
	}
	// If args is a string, it might contain the full command
	if argStr, ok := args["args"].(string); ok {
		// Extract first word as command
		parts := strings.Fields(argStr)
		if len(parts) > 0 {
			return parts[0]
		}
	}
	// If args is an array, first element might be command
	if argList, ok := args["args"].([]string); ok && len(argList) > 0 {
		return argList[0]
	}
	return ""
}

// isCommandBlacklisted checks if the command matches any in the blacklist.
// The blacklist is a comma-separated string of command names (basenames).
func isCommandBlacklisted(args map[string]any, blacklist string) bool {
	if blacklist == "" {
		return false
	}
	cmd := getCommandFromArgs(args)
	if cmd == "" {
		return false
	}
	// Get basename of command (strip path and any suffixes)
	base := path.Base(cmd)
	// Remove any arguments that might be appended
	// e.g., "grep -n" -> "grep"
	base = strings.Fields(base)[0]
	if base == "" {
		return false
	}
	// Check against blacklist
	for _, denied := range strings.Split(blacklist, ",") {
		denied = strings.TrimSpace(denied)
		if denied == "" {
			continue
		}
		if base == denied {
			return true
		}
	}
	return false
}

// isCommandInAskAlwaysList checks if the command matches any in the ask-always list.
// The list is a comma-separated string of command names (basenames) that should
// always require human approval before execution.
func isCommandInAskAlwaysList(args map[string]any, commandList string) bool {
	if commandList == "" {
		return false
	}
	cmd := getCommandFromArgs(args)
	if cmd == "" {
		return false
	}
	// Get basename of command (strip path and any suffixes)
	base := path.Base(cmd)
	// Remove any arguments that might be appended
	// e.g., "rm -rf" -> "rm"
	base = strings.Fields(base)[0]
	if base == "" {
		return false
	}
	// Check against the ask-always list
	for _, cmdName := range strings.Split(commandList, ",") {
		cmdName = strings.TrimSpace(cmdName)
		if cmdName == "" {
			continue
		}
		if base == cmdName {
			return true
		}
	}
	return false
}

func loadPolicy(ctx context.Context, src PolicySource, tenantID, policyPath string) (*Policy, error) {
	data, err := src.ReadPolicy(ctx, tenantID, policyPath)
	if err != nil {
		return nil, fmt.Errorf("read hitl policy %q: %w", policyPath, err)
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse hitl policy %q: %w", policyPath, err)
	}
	if err := rejectUnknownComputeFields(data); err != nil {
		return nil, fmt.Errorf("invalid hitl policy %q: %w", policyPath, err)
	}
	if err := validatePolicy(&p); err != nil {
		return nil, fmt.Errorf("invalid hitl policy %q: %w", policyPath, err)
	}
	return &p, nil
}

// rejectUnknownComputeFields strict-decodes JUST the policy's "compute" sub-object
// so a typo in a NEW bound (maxTurn, onExhaust) fails the policy to load rather
// than silently running the mission unbounded on the field the operator thought
// they set. The strictness is deliberately scoped to the block being introduced:
// the rest of the policy stays laxly parsed (json.Unmarshal above), so every
// existing policy carrying an incidental extra top-level key — a "//"-style comment
// note, a future field — keeps loading exactly as before. Only "compute" is held to
// deny-unknown-fields, and only when it is present.
func rejectUnknownComputeFields(data []byte) error {
	var probe struct {
		Compute json.RawMessage `json:"compute"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		// A malformed document was already reported by the top-level Unmarshal in
		// loadPolicy; nothing to add here.
		return nil
	}
	if len(probe.Compute) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(probe.Compute))
	dec.DisallowUnknownFields()
	var cb ComputeBounds
	if err := dec.Decode(&cb); err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	return nil
}

// validatePolicy checks semantic constraints that cannot be expressed in the JSON schema.
func validatePolicy(p *Policy) error {
	validActions := map[Action]bool{ActionAllow: true, ActionApprove: true, ActionDeny: true}
	if p.DefaultAction != "" && !validActions[p.DefaultAction] {
		return fmt.Errorf("unknown default_action %q", p.DefaultAction)
	}
	for i, r := range p.Rules {
		if !validActions[r.Action] {
			return fmt.Errorf("rule %d: unknown action %q", i, r.Action)
		}
		if r.OnTimeout == ActionAllow {
			return fmt.Errorf("rule %d: on_timeout=%q is not permitted (would silently bypass approval)", i, ActionAllow)
		}
		if r.OnTimeout != "" && !validActions[r.OnTimeout] {
			return fmt.Errorf("rule %d: unknown on_timeout %q", i, r.OnTimeout)
		}
		for j, c := range r.When {
			switch c.Op {
			case OpEq, OpHost, OpCommandBlacklist, OpCommandAskAlways, OpNoCommandSubstitution:
			case OpGlob:
				if err := validateGlobValue(c.Value); err != nil {
					return fmt.Errorf("rule %d, condition %d: %w", i, j, err)
				}
			default:
				return fmt.Errorf("rule %d, condition %d: unknown op %q", i, j, c.Op)
			}
		}
	}
	if p.Attention != nil {
		if err := validateAttentionBounds(p.Attention); err != nil {
			return err
		}
	}
	if p.Compute != nil {
		if err := validateComputeBounds(p.Compute); err != nil {
			return err
		}
	}
	return nil
}

// validateComputeBounds checks the envelope's compute block for shape: every
// ceiling non-negative and within its defensive cap, the onExhausted value known,
// and each allowlist within bounds. It is HARD on shape and silent on tightness —
// it will not tell an operator their maxTurns is too small, only that it is not a
// negative or an absurd one — the same stance the plan/handover validators take.
func validateComputeBounds(c *ComputeBounds) error {
	if err := validateComputeCeiling("maxTurns", c.MaxTurns, maxComputeTurns); err != nil {
		return err
	}
	if err := validateComputeCeiling("maxToolCalls", c.MaxToolCalls, maxComputeToolCalls); err != nil {
		return err
	}
	if err := validateComputeCeiling("maxTokens", c.MaxTokens, maxComputeTokens); err != nil {
		return err
	}
	switch c.OnExhausted {
	case "", OnExhaustedFinishStuck, OnExhaustedPauseAsk:
	default:
		return fmt.Errorf("compute: unknown onExhausted %q (must be finish_stuck or pause_ask)", c.OnExhausted)
	}
	if err := validateComputeAllowlist("modelAllowlist", c.ModelAllowlist); err != nil {
		return err
	}
	return validateComputeAllowlist("backendAllowlist", c.BackendAllowlist)
}

func validateComputeCeiling(name string, v, max int) error {
	if v < 0 {
		return fmt.Errorf("compute: %s must not be negative (got %d)", name, v)
	}
	if v > max {
		return fmt.Errorf("compute: %s is out of range (got %d, max %d)", name, v, max)
	}
	return nil
}

func validateComputeAllowlist(name string, entries []string) error {
	if len(entries) > maxComputeAllowlist {
		return fmt.Errorf("compute: %s has too many entries (%d, max %d)", name, len(entries), maxComputeAllowlist)
	}
	for i, e := range entries {
		if strings.TrimSpace(e) == "" {
			return fmt.Errorf("compute: %s entry %d is empty", name, i)
		}
		if len(e) > maxComputeAllowlistEntryBytes {
			return fmt.Errorf("compute: %s entry %d exceeds max length (%d bytes, max %d)", name, i, len(e), maxComputeAllowlistEntryBytes)
		}
	}
	return nil
}

// validateGlobValue rejects glob patterns that would silently fail to match at
// runtime: unbalanced braces (path.Match would treat "{" literally) and brace
// expressions that explode past the expansion cap. A rejected policy fails to
// load, so evaluation falls back to the built-in default rather than running
// with a deny rule that never fires.
func validateGlobValue(value string) error {
	depth := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return fmt.Errorf("unbalanced '}' in glob %q", value)
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced '{' in glob %q", value)
	}
	if len(expandGlobBraces(value)) >= maxGlobExpansions {
		return fmt.Errorf("glob %q expands past the %d-pattern limit", value, maxGlobExpansions)
	}
	return nil
}

// secretDenyRules is the universal known-bad prefix: paths that never belong
// in any agent workflow on any machine (credential stores, key material,
// shell/persistence init, system dirs). It is kept in sync with
// hitl-policy-acp.json by TestSeededACPPolicySecretInvariant.
func secretDenyRules() []Rule {
	const allTools = "*"
	q := func(g string) Rule {
		return Rule{Tools: "local_fs", Tool: allTools, Action: ActionDeny, When: []Condition{{Key: "path", Op: OpGlob, Value: g}}}
	}
	w := func(g string) []Rule {
		return []Rule{
			{Tools: "local_fs", Tool: "write_file", Action: ActionDeny, When: []Condition{{Key: "path", Op: OpGlob, Value: g}}},
			{Tools: "local_fs", Tool: "sed", Action: ActionDeny, When: []Condition{{Key: "path", Op: OpGlob, Value: g}}},
		}
	}
	rules := []Rule{
		q("**/{.ssh,.gnupg,.aws,.azure,.kube,.config/gcloud,.config/doctl,.oci}/**"),
		q("**/{.password-store,.local/share/keyrings,Library/Keychains,.config/1Password,.config/Bitwarden,.config/keepassxc}/**"),
		q("**/{.electrum,.bitcoin,.ethereum/keystore}/**"),
		q("**/{wallet.dat,*.kdbx}"),
		q("**/{.mozilla,.config/google-chrome,.config/chromium,.config/BraveSoftware}/**"),
		q("**/Library/Application Support/{Google/Chrome,Firefox,BraveSoftware}/**"),
		q("**/{.bash_history,.zsh_history,.python_history,.netrc,.git-credentials,.npmrc,.pypirc}"),
		q("**/.docker/config.json"),
		q("**/{id_rsa,id_dsa,id_ecdsa,id_ed25519}*"),
	}
	rules = append(rules, w("**/{.ssh,.gnupg}/**")...)
	rules = append(rules, w("**/{.config/autostart,.config/systemd/user,Library/LaunchAgents,Library/LaunchDaemons}/**")...)
	rules = append(rules, w("**/{.bashrc,.zshrc,.profile,.bash_profile,.zprofile,.bash_login,.zshenv,.kshrc,crontab,.crontab}")...)
	rules = append(rules, w("**/.config/fish/config.fish")...)
	rules = append(rules, w("/{etc,usr,bin,sbin,boot,lib,lib64,opt,System}/**")...)
	rules = append(rules, w("**/hitl-policy*.json")...)
	return rules
}

func defaultPolicy() *Policy {
	return &Policy{
		DefaultAction: ActionApprove,
		Rules: append(secretDenyRules(), []Rule{
			{Tools: "local_fs", Tool: "read_file", Action: ActionAllow},
			{Tools: "local_fs", Tool: "read_file_range", Action: ActionAllow},
			{Tools: "local_fs", Tool: "list_dir", Action: ActionAllow},
			{Tools: "local_fs", Tool: "grep", Action: ActionAllow},
			{Tools: "local_fs", Tool: "stat_file", Action: ActionAllow},
			{Tools: "local_fs", Tool: "count_stats", Action: ActionAllow},
			{Tools: "local_fs", Tool: "write_file", Action: ActionApprove},
			{Tools: "local_fs", Tool: "sed", Action: ActionApprove},
			// local_shell: block commands that are never allowed under any circumstance.
			// Values must be bare basenames — the matcher extracts path.Base(command) and compares
			// exactly, so multi-word entries like "rm -rf" would silently never fire.
			{Tools: "local_shell", Tool: "local_shell", Action: ActionDeny, When: []Condition{{Key: "command", Op: OpCommandBlacklist, Value: "mkfs,mke2fs,fdisk,shred,wipefs"}}},
			// local_shell: require approval for dangerous commands (ask-always list)
			{Tools: "local_shell", Tool: "local_shell", Action: ActionApprove, When: []Condition{{Key: "command", Op: OpCommandAskAlways, Value: "rm,sudo,dd,chmod,chown,mv,cp,>:,>>"}}},
			// local_shell: require approval for command injection patterns
			{Tools: "local_shell", Tool: "local_shell", Action: ActionApprove, When: []Condition{{Key: "args", Op: OpNoCommandSubstitution, Value: ""}}},
			// local_shell: default to requiring approval (fail-closed safety)
			{Tools: "local_shell", Tool: "local_shell", Action: ActionApprove},
			// shell_session: reading scrollback is reference-only and never gated;
			// submitting a line (shell_session_run) is gated exactly like local_shell,
			// falling through to DefaultAction (approve) below.
			{Tools: "shell_session", Tool: "shell_session_read", Action: ActionAllow},
			{Tools: "webtools", Tool: "web_get", Action: ActionAllow},
			{Tools: "webtools", Tool: "web_head", Action: ActionAllow},
			{Tools: "webtools", Tool: "web_post", Action: ActionApprove},
			{Tools: "webtools", Tool: "web_put", Action: ActionApprove},
			{Tools: "webtools", Tool: "web_patch", Action: ActionApprove},
			{Tools: "webtools", Tool: "web_delete", Action: ActionApprove},
			{Tools: "echo", Action: ActionAllow},
			{Tools: "print", Action: ActionAllow},
		}...),
	}
}
