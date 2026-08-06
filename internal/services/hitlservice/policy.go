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

// PolicySchemaVersion is the envelope wire version this binary can load, the
// taskengine.CheckpointSchemaVersion of the policy document. Bump it only
// together with the migration a document written under the previous version
// needs, never alone: an unmigratable version is refused, not guessed at.
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

	// Detail is the human-readable cause the matched rule's condition found
	// (e.g. which shell command tripped it), mirroring
	// EvaluationResult.Detail. Empty when the rule (or DefaultAction) has no
	// such cause to report.
	Detail string

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
	// Version is the envelope's wire version (see PolicySchemaVersion).
	// Absent (0) means PolicySchemaVersion: every shipped preset and every
	// user-authored policy predates the field and must keep loading
	// unchanged, so absence can never be a load failure. The top-level decode
	// is lax (only the named sub-objects run DisallowUnknownFields), so an
	// older binary reading a versioned document ignores the key rather than
	// refusing it — which is why the refusal has to be a validation, not a
	// decode error.
	Version       int            `json:"version,omitempty"`
	DefaultAction Action         `json:"default_action,omitempty"`
	Rules         []Rule         `json:"rules"`
	Compute       *ComputeBounds `json:"compute,omitempty"`
	// Attention is the optional attention half: who may answer a unit's
	// question (see AttentionBounds). Nil means a human must.
	Attention *AttentionBounds `json:"attention,omitempty"`
	// TrustedBinaries gates every allow a command_prefix_allowlist would
	// grant on the identity and integrity of the binary the named command
	// resolves to (see TrustedBinaries). Nil is inert.
	TrustedBinaries *TrustedBinaries `json:"trusted_binaries,omitempty"`
}

// OnExhausted names what a mission does when it crosses a compute bound —
// the compute analogue of Rule.OnTimeout.
type OnExhausted string

const (
	// OnExhaustedFinishStuck finishes the mission at StatusStuck; the
	// default and only implemented behavior.
	OnExhaustedFinishStuck OnExhausted = "finish_stuck"
	// OnExhaustedPauseAsk is rejected by validatePolicy: it was declared
	// but never implemented, and was silently honored as
	// OnExhaustedFinishStuck before it became an error.
	OnExhaustedPauseAsk OnExhausted = "pause_ask"
)

// ComputeBounds is the envelope's compute half: a ceiling on a mission's
// total compute, alongside the per-tool action rules above. Every bound is a
// ceiling and opt-in — zero/absent is unbounded, and bounds only ever
// restrict. Exhaustion is never silent (see OnExhausted).
//
// MaxTurns is enforced host-side, and only 1 has an effect: it drops the one
// runtime nudge turn, since the dispatcher never issues more than two (see
// validateMaxTurns, which refuses any other non-zero value). MaxToolCalls is validated but not enforced
// by any shipped host: its one enforcement seam is the unattended permission
// answerer, which no shipped host wires. MaxTokens is best-effort, enforced
// only when the unit reports usage. ModelAllowlist and BackendAllowlist are
// enforced at the resolution seam, covering chat, prompt, stream, and embed.
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
	trusted   *TrustedBinaries
	readOnce  bool
	reading   shellReading
	// trustNote is the first binary refusal that withdrew an allow. It
	// outlives the rule that triggered it — the rule stops matching, so its
	// own matchNote is discarded — and is attached to the resulting ask (see
	// detailWithTrustNote) so the card says which binary was not trusted.
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

// trusts is the allow path's last gate: every command word an allow would
// bless must resolve to a declared binary (see TrustedBinaries). It can only
// withdraw an allow, so a policy without declarations answers exactly as before.
func (e *evalScope) trusts(names []string) bool {
	if !e.trusted.enforced() {
		return true
	}
	if len(names) == 0 {
		// An allow whose program word cannot even be named is not one this
		// gate can vouch for.
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

// noteTrustRefusal keeps the FIRST refusal: it is the one the operator should
// fix first, and a later rule's identical finding adds nothing.
func (e *evalScope) noteTrustRefusal(msg string) {
	if e.trustNote == "" {
		e.trustNote = msg
	}
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

// detailWithTrustNote attaches a withdrawn allow's cause to the ask that
// resulted — the card a human is looking at wondering why this stopped.
// Attached only to an approve: an allow means the refusal decided nothing,
// and a deny already carries the stronger reason it was denied for.
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

// conditionMatches takes the rule's action because one operator's behavior
// depends on it: the trusted-binary gate may only withdraw an ALLOW. Applied
// to a rule that denies or asks, withdrawing the match would let the call
// through — a widening, and the one shape this whole layer must never have.
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
//
// Whichever path grants it, the allow is then gated on the identity and
// integrity of the binaries the names resolve to (see TrustedBinaries): a
// prefix pins a NAME, and PATH decides what that name is. That gate applies
// ONLY when the rule allows — withdrawing the match from a rule that denies
// or asks would let the call through, which is the one thing this may not do.
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

// tokenizerProgramWords is the program word of a plain argv call as WRITTEN —
// before commandTokens flattens it to a basename, since "/usr/bin/git" and
// "git" resolve differently and the resolution is the point.
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

// structuralProgramWords is every program word the structural reading named.
// Only reached on the upgrade path, where the audit guarantees each one is a
// fully literal word.
func structuralProgramWords(r shellReading) []string {
	out := make([]string, 0, len(r.commands))
	for _, cmd := range r.commands {
		if cmd.name == "" {
			// Unnameable here means the upgrade path let something through it
			// could not read; refuse rather than vouch for it.
			return nil
		}
		out = append(out, cmd.name)
	}
	return out
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
	raw[0] = allowlistProgramWord(raw[0])
	return raw, true
}

// allowlistProgramWord normalizes a program word for prefix matching.
//
// An absolute path keeps the basename rule: /usr/bin/git is the git a bare
// "git" entry meant, and pinning identity for that case is trusted_binaries'
// job, not the matcher's.
//
// A RELATIVE pathed word does not: "./node_modules/.bin/eslint" or
// "tools/cat" names a file inside the very tree the agent was pointed at, so
// reducing it to "eslint"/"cat" would let a checked-out repo or an unpacked
// dependency inherit an allow meant for the operator's own toolchain. It is
// compared as written instead, which no bare-name entry equals. The refusal
// is not a denial: the call falls through to the next tier, which asks. An
// operator who means a repo-local binary writes that path in the allowlist.
func allowlistProgramWord(word string) string {
	if !filepath.IsAbs(word) && strings.ContainsAny(word, `/\`) {
		return word
	}
	return path.Base(word)
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
	if err := rejectUnknownSubObjectFields(data, "trusted_binaries", &TrustedBinaries{}); err != nil {
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
	return rejectUnknownSubObjectFields(data, "compute", &ComputeBounds{})
}

// rejectUnknownSubObjectFields strict-decodes one named sub-object of the
// policy document into `into`, so a typo inside an enforcement block fails
// the policy to load (falling back to the rule-free approve-everything
// default) rather than silently disarming it.
func rejectUnknownSubObjectFields(data []byte, field string, into any) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		// Already reported by the top-level Unmarshal in loadPolicy.
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

// validatePolicy checks semantic constraints that cannot be expressed in the JSON schema.
func validatePolicy(p *Policy) error {
	// Version first: every check below reads fields whose meaning is only
	// pinned by the version that declared them.
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

// validatePolicyVersion refuses a version this binary cannot load, rather
// than decoding a document whose fields may have changed meaning under it.
// Absent (0) is PolicySchemaVersion, so a policy written before the field
// existed loads byte-identically. The older-with-no-migration arm is
// unreachable while PolicySchemaVersion is 1 and becomes the seam a bump
// must fill.
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

// validateComputeBounds checks shape only — non-negative and within its
// defensive cap — never tightness.
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

// validateMaxTurns admits only the two values that change what a mission
// does. The dispatcher issues at most two prompt turns — the unit's own turn
// and one runtime nudge when it went mute — so 1 suppresses the nudge and
// anything above it names a turn the dispatcher was never going to take.
// Refusing those is the whole point: accepting them silently is how a shipped
// preset came to declare maxTurns: 8, a ceiling nothing could ever reach.
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
