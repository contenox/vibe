package hitlservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// PolicySchemaVersion is the wire version of the policy document this binary can load.
const PolicySchemaVersion = 1

// ErrPolicyVersion reports an envelope whose declared version this binary
// cannot load — newer than it knows, or older with no migration.
var ErrPolicyVersion = errors.New("hitlservice: unsupported hitl policy version")

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
type ApprovalRequest struct {
	ToolCallID string
	ToolsName  string
	ToolName   string
	Args       map[string]any
	Diff       string
	DiffOld    string
	DiffNew    string

	// PolicyName, MatchedRule, TimeoutS, and OnTimeout carry the policy
	// verdict that produced this ask; TimeoutS<=0 means no rule timeout and
	// OnTimeout=="" means default deny.
	PolicyName  string
	MatchedRule *int
	TimeoutS    int
	OnTimeout   Action

	// Detail is the human-readable cause the matched rule's condition found,
	// mirroring EvaluationResult.Detail; empty when there is no such cause.
	Detail string

	// InstanceID, SessionID, AgentName, and MissionID attribute the ask to
	// the fleet unit that raised it; all four are optional and ignored by
	// the attached-session path.
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
	// OpGlob matches the argument value against a glob pattern, both
	// normalized with path.Clean to prevent traversal; supports *, ?, and
	// ** (across separators).
	OpGlob ConditionOp = "glob"
	// OpHost parses the argument as a URL and matches its host against
	// comma-separated patterns in Value; IP literals match exactly, bare
	// names match the host and any subdomain.
	OpHost ConditionOp = "host"
	// OpCommandBlacklist matches the command basename against a
	// comma-separated denylist, including every command a readable shell
	// line runs.
	OpCommandBlacklist ConditionOp = "command_blacklist"
	// OpCommandAskAlways matches like OpCommandBlacklist but pairs with
	// action:"approve" instead of deny.
	OpCommandAskAlways ConditionOp = "command_ask_always"
	// OpNoCommandSubstitution blocks shell substitution patterns ($(),
	// backticks, <(), >()), including via AST for a readable shell line.
	OpNoCommandSubstitution ConditionOp = "no_command_substitution"
	// OpCommandPrefixAllowlist matches the call's command line, as tokens,
	// against comma-separated safe prefixes, refusing any control or
	// substitution character.
	OpCommandPrefixAllowlist ConditionOp = "command_prefix_allowlist"
)

// Condition is a single key/op/value predicate applied to the args of a tool call.
type Condition struct {
	Key   string      `json:"key"`
	Op    ConditionOp `json:"op" jsonschema:"required"`
	Value string      `json:"value"`
}

// Rule matches a tools+tool pair (with optional AND-conditions) and assigns
// an action.
type Rule struct {
	Tools  string      `json:"tools"`
	Tool   string      `json:"tool"`
	When   []Condition `json:"when,omitempty"`
	Action Action      `json:"action" jsonschema:"required"`
	// TimeoutS is how long to wait for a human response when Action is
	// ActionApprove; 0 means block indefinitely.
	TimeoutS int `json:"timeout_s,omitempty"`
	// OnTimeout is the fallback when the approval window expires; only
	// "deny" or "approve" is valid (allow would silently bypass approval).
	OnTimeout Action `json:"on_timeout,omitempty"`
}

// Policy is the top-level document stored as hitl-policy.json in the VFS;
// rules are evaluated in order, first match wins, and DefaultAction applies
// when none match (fail-closed to "approve" when absent).
type Policy struct {
	// Version is the envelope's wire version (see PolicySchemaVersion);
	// absent (0) means PolicySchemaVersion.
	Version       int            `json:"version,omitempty"`
	DefaultAction Action         `json:"default_action,omitempty"`
	Rules         []Rule         `json:"rules"`
	Compute       *ComputeBounds `json:"compute,omitempty"`
	// Attention is the optional attention half: who may answer a unit's
	// question (see AttentionBounds); nil means a human must.
	Attention *AttentionBounds `json:"attention,omitempty"`
	// TrustedBinaries gates every allow a command_prefix_allowlist would
	// grant on the identity and integrity of the resolved binary (see
	// TrustedBinaries); nil is inert.
	TrustedBinaries *TrustedBinaries `json:"trusted_binaries,omitempty"`
}

// OnExhausted names what a mission does when it crosses a compute bound —
// the compute analogue of Rule.OnTimeout.
type OnExhausted string

const (
	// OnExhaustedFinishStuck finishes the mission at StatusStuck; the
	// default and only implemented behavior.
	OnExhaustedFinishStuck OnExhausted = "finish_stuck"
	// OnExhaustedPauseAsk is rejected by validatePolicy as not implemented.
	OnExhaustedPauseAsk OnExhausted = "pause_ask"
)

// ComputeBounds is the envelope's compute half: an opt-in ceiling on a
// mission's total compute, alongside the per-tool action rules; zero/absent
// is unbounded and exhaustion is never silent (see OnExhausted).
type ComputeBounds struct {
	MaxTurns         int         `json:"maxTurns,omitempty"`
	MaxToolCalls     int         `json:"maxToolCalls,omitempty"`
	MaxTokens        int         `json:"maxTokens,omitempty"`
	ModelAllowlist   []string    `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string    `json:"backendAllowlist,omitempty"`
	OnExhausted      OnExhausted `json:"onExhausted,omitempty"`
}

const (
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
	// not self-evident from the args.
	Detail string
}

type evalScope struct {
	args      map[string]any
	shellKind ShellKind
	trusted   *TrustedBinaries
	readOnce  bool
	reading   shellReading
	trustNote string
}

func newEvalScope(ctx context.Context, p *Policy, args map[string]any) *evalScope {
	scope := &evalScope{args: args, shellKind: ShellKindFromContext(ctx)}
	if p != nil {
		scope.trusted = p.TrustedBinaries
	}
	return scope
}

func (e *evalScope) shell() shellReading {
	if !e.readOnce {
		e.reading = analyzeShellArgs(e.shellKind, e.args)
		e.readOnce = true
	}
	return e.reading
}

func (e *evalScope) trusts(names []string) bool {
	if !e.trusted.enforced() {
		return true
	}
	if len(names) == 0 {
		e.noteTrustRefusal("the allowed command could not be named for a trusted-binary check — allow refused")
		return false
	}
	for _, name := range names {
		if msg := e.trusted.verifyCommand(name); msg != "" {
			e.noteTrustRefusal(msg)
			return false
		}
	}
	return true
}

func (e *evalScope) noteTrustRefusal(msg string) {
	if e.trustNote == "" {
		e.trustNote = msg
	}
}

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

func evaluate(ctx context.Context, p *Policy, toolsName, toolName string, args map[string]any) EvaluationResult {
	scope := newEvalScope(ctx, p, args)
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
				Detail:      detailWithTrustNote(note.detail(), r.Action, scope),
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
		Detail: detailWithTrustNote("", defaultAction, scope),
	}
}

func detailWithTrustNote(base string, action Action, scope *evalScope) string {
	if action != ActionApprove || scope.trustNote == "" {
		return base
	}
	if base == "" {
		return scope.trustNote
	}
	return base + "; " + scope.trustNote
}

func ruleMatches(r Rule, toolsName, toolName string, scope *evalScope, note *matchNote) bool {
	toolsOK := r.Tools == "" || r.Tools == "*" || r.Tools == toolsName
	toolOK := r.Tool == "" || r.Tool == "*" || r.Tool == toolName
	if !toolsOK || !toolOK {
		return false
	}
	for _, c := range r.When {
		if !conditionMatches(c, r.Action, scope, note) {
			return false
		}
	}
	return true
}

func conditionMatches(c Condition, action Action, scope *evalScope, note *matchNote) bool {
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
			if commandPrefixAllowMatch(scope, c.Value, action) {
				return true
			}
		}
	}
	return false
}

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

func commandSubstitutionMatch(scope *evalScope) bool {
	return detectCommandSubstitution(scope.args) || structuralCommandSubstitution(scope.shell())
}

func commandPrefixAllowMatch(scope *evalScope, prefixList string, action Action) bool {
	gate := func(names []string) bool {
		if action != ActionAllow {
			return true
		}
		return scope.trusts(names)
	}
	if isCommandPrefixAllowed(scope.args, prefixList) {
		if structuralContradictsPrefix(scope.shell(), prefixList) {
			return false
		}
		return gate(tokenizerProgramWords(scope.args))
	}
	if !structuralPrefixAllowed(scope.shell(), prefixList) {
		return false
	}
	return gate(structuralProgramWords(scope.shell()))
}

func tokenizerProgramWords(args map[string]any) []string {
	if cmd, ok := args["command"].(string); ok {
		if fields := strings.Fields(cmd); len(fields) > 0 {
			return fields[:1]
		}
	}
	if tokens := argTokens(args["args"]); len(tokens) > 0 {
		return tokens[:1]
	}
	return nil
}

func structuralProgramWords(r shellReading) []string {
	out := make([]string, 0, len(r.commands))
	for _, cmd := range r.commands {
		if cmd.name == "" {
			// Unnameable means the upgrade path let something through unread; refuse rather than vouch.
			return nil
		}
		out = append(out, cmd.name)
	}
	return out
}

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

var commandSubstitutionPatterns = []string{
	"$(",
	"`",
	"<(",
	">(",
	"$[",
	"${}",
	"$((",
}

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

const shellControlChars = ";|&><`\n\r"

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
	raw[0] = allowlistProgramWord(raw[0])
	return raw, true
}

func allowlistProgramWord(word string) string {
	if !filepath.IsAbs(word) && strings.ContainsAny(word, `/\`) {
		return word
	}
	return path.Base(word)
}

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
	if err := rejectUnknownSubObjectFields(data, "trusted_binaries", &TrustedBinaries{}); err != nil {
		return nil, fmt.Errorf("invalid hitl policy %q: %w", policyPath, err)
	}
	if err := validatePolicy(&p); err != nil {
		return nil, fmt.Errorf("invalid hitl policy %q: %w", policyPath, err)
	}
	return &p, nil
}

func rejectUnknownComputeFields(data []byte) error {
	return rejectUnknownSubObjectFields(data, "compute", &ComputeBounds{})
}

func rejectUnknownSubObjectFields(data []byte, field string, into any) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	raw, ok := probe[field]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func validatePolicy(p *Policy) error {
	// Version is checked first: later checks assume version-pinned field meanings.
	if err := validatePolicyVersion(p.Version); err != nil {
		return err
	}
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
	return validateTrustedBinaries(p.TrustedBinaries)
}

func validatePolicyVersion(v int) error {
	switch {
	case v == 0 || v == PolicySchemaVersion:
		return nil
	case v < 0:
		return fmt.Errorf("%w: version must not be negative (got %d)", ErrPolicyVersion, v)
	case v > PolicySchemaVersion:
		return fmt.Errorf("%w: policy declares version %d but this binary knows only %d — update contenox to load it", ErrPolicyVersion, v, PolicySchemaVersion)
	default:
		return fmt.Errorf("%w: policy declares version %d, which this binary can no longer load (current %d) — no migration is registered for it", ErrPolicyVersion, v, PolicySchemaVersion)
	}
}

func validateComputeBounds(c *ComputeBounds) error {
	if err := validateMaxTurns(c.MaxTurns); err != nil {
		return err
	}
	if err := validateComputeCeiling("maxToolCalls", c.MaxToolCalls, maxComputeToolCalls); err != nil {
		return err
	}
	if err := validateComputeCeiling("maxTokens", c.MaxTokens, maxComputeTokens); err != nil {
		return err
	}
	switch c.OnExhausted {
	case "", OnExhaustedFinishStuck:
	case OnExhaustedPauseAsk:
		return fmt.Errorf("compute: %s is not implemented; use %s", OnExhaustedPauseAsk, OnExhaustedFinishStuck)
	default:
		return fmt.Errorf("compute: unknown onExhausted %q (must be %s)", c.OnExhausted, OnExhaustedFinishStuck)
	}
	if err := validateComputeAllowlist("modelAllowlist", c.ModelAllowlist); err != nil {
		return err
	}
	return validateComputeAllowlist("backendAllowlist", c.BackendAllowlist)
}

func validateMaxTurns(v int) error {
	switch v {
	case 0, 1:
		return nil
	default:
		return fmt.Errorf("compute: maxTurns is %d, but a mission runs at most two prompt turns (its own, plus one runtime nudge when it reports nothing): only 1 has an effect — it drops the nudge — and omitting the field keeps it. Remove maxTurns or set it to 1", v)
	}
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

func defaultPolicy() *Policy {
	return &Policy{DefaultAction: ActionApprove}
}
