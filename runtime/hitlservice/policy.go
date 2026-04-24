package hitlservice

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/contenox/contenox/runtime/vfsservice"
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
	ToolsName string
	ToolName string
	Args     map[string]any
	Diff     string
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
	Tools      string      `json:"tools"`
	Tool      string      `json:"tool"`
	When      []Condition `json:"when,omitempty"`
	Action    Action      `json:"action"`
	// TimeoutS is the number of seconds to wait for a human response when Action is
	// ActionApprove. Zero means no timeout (block indefinitely until ctx is cancelled).
	TimeoutS  int    `json:"timeout_s,omitempty"`
	// OnTimeout is the fallback action when the approval window expires.
	// Only "deny" and "approve" are valid (allow would silently bypass approval).
	OnTimeout Action `json:"on_timeout,omitempty"`
}

// Policy is the top-level document stored as hitl-policy.json in the VFS.
// Rules are evaluated in order; the first matching rule wins.
// DefaultAction is applied when no rule matches; it defaults to "allow" when absent
// so existing deployments without a policy file keep the original behaviour.
type Policy struct {
	DefaultAction Action `json:"default_action,omitempty"`
	Rules         []Rule `json:"rules"`
}

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
		defaultAction = ActionAllow
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
	valStr := fmt.Sprintf("%v", val)
	switch c.Op {
	case OpEq:
		return valStr == c.Value
	case OpGlob:
		return globMatch(c.Value, valStr)
	default:
		return false
	}
}

// globMatch reports whether s matches the glob pattern.
// Both pattern and s are normalized with path.Clean before comparison to prevent
// path-traversal bypasses. Supports *, ?, and ** (which matches across path separators).
func globMatch(pattern, s string) bool {
	pattern = path.Clean(pattern)
	s = path.Clean(s)

	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	if !strings.Contains(pattern, "**") {
		matched, err := path.Match(pattern, s)
		return err == nil && matched
	}
	return matchDoubleGlob(pattern, s)
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

func loadPolicy(ctx context.Context, vfs vfsservice.Service, policyPath string) (*Policy, error) {
	f, err := vfs.GetFileByID(ctx, policyPath)
	if err != nil {
		return nil, fmt.Errorf("read hitl policy %q: %w", policyPath, err)
	}
	var p Policy
	if err := json.Unmarshal(f.Data, &p); err != nil {
		return nil, fmt.Errorf("parse hitl policy %q: %w", policyPath, err)
	}
	if err := validatePolicy(&p); err != nil {
		return nil, fmt.Errorf("invalid hitl policy %q: %w", policyPath, err)
	}
	return &p, nil
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
			if c.Op != OpEq && c.Op != OpGlob {
				return fmt.Errorf("rule %d, condition %d: unknown op %q", i, j, c.Op)
			}
		}
	}
	return nil
}

// defaultPolicy returns the built-in policy used when hitl-policy.json is absent.
// It mirrors the legacy defaultHITLTools set: write_file, sed, and local_shell require approval.
func defaultPolicy() *Policy {
	return &Policy{
		Rules: []Rule{
			{Tools: "local_fs", Tool: "write_file", Action: ActionApprove},
			{Tools: "local_fs", Tool: "sed", Action: ActionApprove},
			{Tools: "local_shell", Tool: "local_shell", Action: ActionApprove},
		},
	}
}
