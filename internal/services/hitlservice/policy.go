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
// Diff is populated for file-mutation tools to show the unified diff.
type ApprovalRequest struct {
	ToolCallID string
	ToolsName  string
	ToolName   string
	Args       map[string]any
	Diff       string
	DiffOld    string
	DiffNew    string

	// PolicyName, MatchedRule, TimeoutS, and OnTimeout carry the policy
	// verdict that produced this ask, persisted onto the durable row. Zero
	// values are safe: TimeoutS<=0 means no rule timeout; OnTimeout==""
	// means default deny.
	PolicyName  string
	MatchedRule *int
	TimeoutS    int
	OnTimeout   Action

	// InstanceID, SessionID, AgentName, and MissionID attribute the ask to
	// the fleet unit that raised it. All four are optional, supplied by the
	// unattended-permission answerer; the attached-session path ignores them.
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
	// OpGlob matches the argument value against a glob pattern. Both value
	// and pattern are normalized with path.Clean before matching, preventing
	// path-traversal bypass. Supports *, ? (single char), and ** (across separators).
	OpGlob ConditionOp = "glob"
	// OpHost parses the argument as a URL and matches its host against
	// comma-separated patterns in Value — SSRF-style host denial that a
	// trailing :port or path cannot evade. IP literals match exactly; bare
	// names match the host and any subdomain.
	OpHost ConditionOp = "host"
	// OpCommandBlacklist matches the command basename against a
	// comma-separated denylist. It catches the call's own first token and,
	// for a shell line the analyzer can read (see shellstructure.go), every
	// command that line runs — structure only ever adds catches.
	OpCommandBlacklist ConditionOp = "command_blacklist"
	// OpCommandAskAlways matches like OpCommandBlacklist but pairs with
	// action:"approve" instead of deny, for safety-critical commands.
	OpCommandAskAlways ConditionOp = "command_ask_always"
	// OpNoCommandSubstitution blocks shell substitution patterns ($(),
	// backticks, <(), >()); for a readable shell line it also checks the AST
	// for a CmdSubst node, which quoting tricks cannot evade.
	OpNoCommandSubstitution ConditionOp = "no_command_substitution"
	// OpCommandPrefixAllowlist matches the call's command line, as tokens,
	// against comma-separated safe prefixes (action:"allow") — e.g. "git log"
	// covers `git log --oneline` but not `git clean -fd`. It matches only a
	// plain argv call: shell mode, or any control/substitution character
	// (; | & > < newline, backtick, $( ) in any token, refuses the match
	// outright, so `git status && rm -rf ~` cannot enter through a `git
	// status` entry. For a shell line the structural analyzer can fully
	// parse (see shellstructure.go), the same list gets a second chance: it
	// matches when every command in the line is on it.
	OpCommandPrefixAllowlist ConditionOp = "command_prefix_allowlist"
)

// Condition is a single key/op/value predicate applied to the args of a tool call.
type Condition struct {
	Key   string      `json:"key"`
	Op    ConditionOp `json:"op"`
	Value string      `json:"value"`
}

// Rule matches a tools+tool pair (with optional AND-conditions) and assigns
// an action.
type Rule struct {
	Tools  string      `json:"tools"`
	Tool   string      `json:"tool"`
	When   []Condition `json:"when,omitempty"`
	Action Action      `json:"action"`
	// TimeoutS is how long to wait for a human response when Action is
	// ActionApprove; 0 means block indefinitely.
	TimeoutS int `json:"timeout_s,omitempty"`
	// OnTimeout is the fallback when the approval window expires; only
	// "deny" or "approve" is valid (allow would silently bypass approval).
	OnTimeout Action `json:"on_timeout,omitempty"`
}

// Policy is the top-level document stored as hitl-policy.json in the VFS.
// Rules are evaluated in order, first match wins; DefaultAction applies when
// none match and is fail-closed to "approve" when absent.
//
// Compute is the optional compute half of the envelope (see ComputeBounds);
// a nil Compute is unbounded, matching pre-existing behavior byte-for-byte.
type Policy struct {
	DefaultAction Action         `json:"default_action,omitempty"`
	Rules         []Rule         `json:"rules"`
	Compute       *ComputeBounds `json:"compute,omitempty"`
	// Attention is the optional attention half: who may answer a unit's
	// question (see AttentionBounds). Nil means a human must.
	Attention *AttentionBounds `json:"attention,omitempty"`
}

// OnExhausted names what a mission does when it crosses a compute bound —
// the compute analogue of Rule.OnTimeout.
type OnExhausted string

const (
	// OnExhaustedFinishStuck finishes the mission at StatusStuck; the
	// default and only behavior enforced today.
	OnExhaustedFinishStuck OnExhausted = "finish_stuck"
	// OnExhaustedPauseAsk is declared but not implemented: until the
	// machinery lands, an envelope that sets it is honored as finish_stuck
	// at the enforcement seam. `contenox vet` warns on it at authoring time.
	OnExhaustedPauseAsk OnExhausted = "pause_ask"
)

// ComputeBounds is the envelope's compute half: a ceiling on a mission's
// total compute, alongside the per-tool action rules above. Every bound is a
// ceiling and opt-in — zero/absent is unbounded, and bounds only ever
// restrict. Exhaustion is never silent (see OnExhausted).
//
// MaxTurns and MaxToolCalls are enforced host-side. MaxTokens is
// best-effort, enforced only when the unit reports usage. ModelAllowlist and
// BackendAllowlist are enforced at the resolution seam, covering chat,
// prompt, stream, and embed.
type ComputeBounds struct {
	MaxTurns         int         `json:"maxTurns,omitempty"`
	MaxToolCalls     int         `json:"maxToolCalls,omitempty"`
	MaxTokens        int         `json:"maxTokens,omitempty"`
	ModelAllowlist   []string    `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string    `json:"backendAllowlist,omitempty"`
	OnExhausted      OnExhausted `json:"onExhausted,omitempty"`
}

// Compute-bound validation caps are defensive, not aesthetic: they reject a
// negative or absurd value (a typo) rather than impose a house style on how
// tight a bound should be.
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
	// Detail names what in the call the matched rule actually caught, when
	// not self-evident from the args — e.g. which command a structural shell
	// reading found.
	Detail string
}

// evalScope carries what condition matchers need beyond args, and caches the
// one structural shell reading so multiple shell rules parse the line once.
type evalScope struct {
	args      map[string]any
	shellKind ShellKind
	readOnce  bool
	reading   shellReading
}

func newEvalScope(ctx context.Context, args map[string]any) *evalScope {
	return &evalScope{args: args, shellKind: ShellKindFromContext(ctx)}
}

func (e *evalScope) shell() shellReading {
	if !e.readOnce {
		e.reading = analyzeShellArgs(e.shellKind, e.args)
		e.readOnce = true
	}
	return e.reading
}

// matchNote is what a condition learned while matching, for EvaluationResult.Detail.
type matchNote struct {
	op      ConditionOp
	command string
}

func (n *matchNote) set(op ConditionOp, command string) {
	if n == nil || command == "" || n.command != "" {
		return
	}
	n.op, n.command = op, command
}

func (n matchNote) detail() string {
	if n.command == "" {
		return ""
	}
	return fmt.Sprintf("shell command %q matched %s", n.command, n.op)
}

// evaluate returns the EvaluationResult for the given tools, tool name, and call args.
func evaluate(ctx context.Context, p *Policy, toolsName, toolName string, args map[string]any) EvaluationResult {
	scope := newEvalScope(ctx, args)
	for i, r := range p.Rules {
		var note matchNote
		if ruleMatches(r, toolsName, toolName, scope, &note) {
			idx := i
			return EvaluationResult{
				Action:      r.Action,
				MatchedRule: &idx,
				Reason:      ReasonMatchedRule,
				TimeoutS:    r.TimeoutS,
				OnTimeout:   r.OnTimeout,
				Detail:      note.detail(),
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

func ruleMatches(r Rule, toolsName, toolName string, scope *evalScope, note *matchNote) bool {
	toolsOK := r.Tools == "" || r.Tools == "*" || r.Tools == toolsName
	toolOK := r.Tool == "" || r.Tool == "*" || r.Tool == toolName
	if !toolsOK || !toolOK {
		return false
	}
	for _, c := range r.When {
		if !conditionMatches(c, scope, note) {
			return false
		}
	}
	return true
}

func conditionMatches(c Condition, scope *evalScope, note *matchNote) bool {
	args := scope.args
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
			if name, hit := commandInListMatch(scope, c.Value); hit {
				note.set(c.Op, name)
				return true
			}
		case OpCommandAskAlways:
			if name, hit := commandInListMatch(scope, c.Value); hit {
				note.set(c.Op, name)
				return true
			}
		case OpNoCommandSubstitution:
			if commandSubstitutionMatch(scope) {
				return true
			}
		case OpCommandPrefixAllowlist:
			if commandPrefixAllowMatch(scope, c.Value) {
				return true
			}
		}
	}
	return false
}

// The four shell operators below share one shape: today's tokenizer answer,
// widened by what the AST structurally shows (see shellstructure.go).

// commandInListMatch backs both OpCommandBlacklist and OpCommandAskAlways.
// It returns the name that matched.
func commandInListMatch(scope *evalScope, list string) (string, bool) {
	if commandBasenameInList(scope.args, list) {
		return commandBasename(scope.args), true
	}
	if cmd, ok := structuralCommandInList(scope.shell(), list); ok {
		name := cmd.display
		if name == "" {
			name = cmd.base
		}
		return name, true
	}
	return "", false
}

// commandSubstitutionMatch combines the textual pattern search with the AST
// question, so quoting tricks that defeat the former are still caught.
func commandSubstitutionMatch(scope *evalScope) bool {
	return detectCommandSubstitution(scope.args) || structuralCommandSubstitution(scope.shell())
}

// commandPrefixAllowMatch is the token-wise prefix match, widened to a
// compound line whose every command is on the same allowlist. The
// tokenizer's own match is revoked when structure shows a command it wasn't
// reading (see structuralContradictsPrefix) — the one place a structural
// reading overrides an allow rather than adding one.
func commandPrefixAllowMatch(scope *evalScope, prefixList string) bool {
	if isCommandPrefixAllowed(scope.args, prefixList) {
		return !structuralContradictsPrefix(scope.shell(), prefixList)
	}
	return structuralPrefixAllowed(scope.shell(), prefixList)
}

// urlHostMatches parses rawURL and reports whether its host equals, or is a
// subdomain of, any pattern in patternsCSV. IP literals match exactly.
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

// conditionValues flattens an argument value into strings a condition is
// tested against, element-wise for slices so stringifying can't hide an entry.
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

// globMatch reports whether s matches pattern; both are path.Clean'd first to
// prevent traversal bypass. Supports *, ?, and ** (across separators).
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
// concrete patterns (Go's path.Match has no brace support). Unbalanced
// braces are literal; expansion is capped at maxGlobExpansions.
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

// matchDoubleGlob handles patterns containing **, which matches zero or more
// path components.
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

// commandSubstitutionPatterns are shell metacharacters that enable command substitution.
var commandSubstitutionPatterns = []string{
	"$(",
	"`",
	"<(",
	">(",
	"$[",
	"${}",
	"$((",
}

// detectCommandSubstitution checks args for shell metacharacters enabling injection.
func detectCommandSubstitution(args map[string]any) bool {
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

// getCommandFromArgs extracts the command string from tool arguments,
// checking both "command" and the first element of "args".
func getCommandFromArgs(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok {
		return cmd
	}
	if argStr, ok := args["args"].(string); ok {
		parts := strings.Fields(argStr)
		if len(parts) > 0 {
			return parts[0]
		}
	}
	if argList, ok := args["args"].([]string); ok && len(argList) > 0 {
		return argList[0]
	}
	return ""
}

// commandBasename extracts the bare program name a rule's command list is
// compared against ("/sbin/mkfs" -> "mkfs", "rm -rf" -> "rm").
func commandBasename(args map[string]any) string {
	cmd := getCommandFromArgs(args)
	if cmd == "" {
		return ""
	}
	fields := strings.Fields(path.Base(cmd))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// commandBasenameInList reports whether the call's command basename appears
// in a comma-separated list; backs both the blacklist and ask-always operators.
func commandBasenameInList(args map[string]any, commandList string) bool {
	if commandList == "" {
		return false
	}
	base := commandBasename(args)
	if base == "" {
		return false
	}
	for _, name := range strings.Split(commandList, ",") {
		if name = strings.TrimSpace(name); name != "" && base == name {
			return true
		}
	}
	return false
}

// shellControlChars disqualify a token from prefix-allowlist matching
// (chaining, piping, redirection, substitution). A lone "$" is excluded:
// without a shell, "$HOME" is a literal argument.
const shellControlChars = ";|&><`\n\r"

// commandTokens flattens a local_shell call into the token sequence a prefix
// is compared against. ok is false when the call is not a plain argv call.
func commandTokens(args map[string]any) (tokens []string, ok bool) {
	if shellModeRequested(args) {
		return nil, false
	}
	var raw []string
	if cmd, isStr := args["command"].(string); isStr && strings.TrimSpace(cmd) != "" {
		raw = append(raw, strings.Fields(cmd)...)
		raw = append(raw, argTokens(args["args"])...)
	} else {
		raw = argTokens(args["args"])
	}
	if len(raw) == 0 {
		return nil, false
	}
	for _, t := range raw {
		if strings.ContainsAny(t, shellControlChars) {
			return nil, false
		}
		for _, pattern := range commandSubstitutionPatterns {
			if strings.Contains(t, pattern) {
				return nil, false
			}
		}
	}
	raw[0] = path.Base(raw[0])
	return raw, true
}

// shellModeRequested reports whether the call asked for the platform shell,
// so a matched allowlist entry cannot be smuggled through a shell string.
func shellModeRequested(args map[string]any) bool {
	switch v := args["shell"].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "y", "on":
			return true
		}
	}
	return false
}

// argTokens flattens an "args" value into tokens: a string is split on
// whitespace, an array is taken element-wise.
func argTokens(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return strings.Fields(t)
	case []string:
		return append([]string(nil), t...)
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

// isCommandPrefixAllowed reports whether the call's command line begins with
// one of the comma-separated safe prefixes. See OpCommandPrefixAllowlist.
func isCommandPrefixAllowed(args map[string]any, prefixList string) bool {
	if strings.TrimSpace(prefixList) == "" {
		return false
	}
	tokens, ok := commandTokens(args)
	if !ok {
		return false
	}
	return prefixListMatchesTokens(tokens, prefixList)
}

// prefixListMatchesTokens reports whether a token line begins with one of
// the comma-separated prefixes. Shared by the tokenizer and structural
// (shellstructure.go) paths, so both are judged by the same semantics.
func prefixListMatchesTokens(tokens []string, prefixList string) bool {
	for _, entry := range strings.Split(prefixList, ",") {
		want := strings.Fields(entry)
		if len(want) == 0 || len(want) > len(tokens) {
			continue
		}
		matched := true
		for i, w := range want {
			if tokens[i] != w {
				matched = false
				break
			}
		}
		if matched {
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

// rejectUnknownComputeFields strict-decodes just the policy's "compute"
// sub-object, so a typo in a new bound fails the policy to load rather than
// silently running the mission unbounded. The rest of the policy stays
// laxly parsed.
func rejectUnknownComputeFields(data []byte) error {
	var probe struct {
		Compute json.RawMessage `json:"compute"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		// Already reported by the top-level Unmarshal in loadPolicy.
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
			case OpEq, OpHost, OpCommandBlacklist, OpCommandAskAlways, OpNoCommandSubstitution, OpCommandPrefixAllowlist:
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

// validateComputeBounds checks shape only — non-negative and within its
// defensive cap — never tightness.
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

// validateGlobValue rejects glob patterns that would silently fail to match:
// unbalanced braces, or brace expressions exploding past the expansion cap.
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

// defaultPolicy is the fallback when the named policy cannot be loaded. It
// must stay rule-free and fail-closed (every call asks a human): a load
// failure must never silently substitute a permissive ruleset. Fix the
// underlying policy file; `contenox vet` explains what is broken.
func defaultPolicy() *Policy {
	return &Policy{DefaultAction: ActionApprove}
}
