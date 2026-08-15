package agentdecl

import (
	"fmt"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missiontools"
)

// PolicySchemaURL is stamped on emitted policies so an editor completes and
// validates them the way it does the shipped presets.
const PolicySchemaURL = "https://contenox.com/schema/hitl-policy-v1.schema.json"

// ErrUnsafePosture reports a source asking for permission prompts to be
// skipped entirely.
type ErrUnsafePosture struct {
	Name    string
	Dialect Dialect
}

func (e *ErrUnsafePosture) Error() string {
	return fmt.Sprintf("agentdecl: %s agent %q asks for permissionMode: bypassPermissions — to skip every approval. "+
		"contenox will not run an agent that way. Use acceptEdits and grant what it actually needs "+
		"under [policy.postures] or [[policy.always_allow]] in agents.toml, where the grant is written down",
		e.Dialect, e.Name)
}

// EmitPolicy widens the agent's single permission setting into the rules that
// govern it. Rules are first-match-wins, so the unconditional denies precede
// every grant. A tool the postures do not name falls to DefaultAction, which
// asks a human rather than proceeding.
func EmitPolicy(ir *AgentIR, cfg Config) (*hitlservice.Policy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if ir.Posture == PostureUnsafe {
		return nil, &ErrUnsafePosture{Name: ir.Name, Dialect: ir.Source.Dialect}
	}

	grants, ok := cfg.Policy.Postures[string(ir.Posture)]
	if !ok {
		return nil, fmt.Errorf("agentdecl: no posture grants configured for %q", ir.Posture)
	}

	rules := make([]hitlservice.Rule, 0, len(cfg.Policy.AlwaysDeny)+len(cfg.Policy.AlwaysAllow)+16)
	rules = append(rules, standingRules(cfg.Policy.AlwaysDeny, hitlservice.ActionDeny)...)
	rules = append(rules, standingRules(cfg.Policy.AlwaysAllow, hitlservice.ActionAllow)...)
	rules = append(rules, missionToolRules(ir)...)

	for _, g := range []struct {
		tools  string
		tool   string
		action string
	}{
		{"local_fs", "read_file", grants.LocalFSRead},
		{"local_fs", "read_file_range", grants.LocalFSRead},
		{"local_fs", "stat_file", grants.LocalFSRead},
		{"local_fs", "list_dir", grants.LocalFSRead},
		{"local_fs", "grep", grants.LocalFSRead},
		{"local_fs", "find_files", grants.LocalFSRead},
		{"local_fs", "count_stats", grants.LocalFSRead},
		{"local_fs", "write_file", grants.LocalFSWrite},
		{"local_fs", "edit_file", grants.LocalFSWrite},
		{"local_fs", "sed", grants.LocalFSWrite},
		{"local_shell", "local_shell", grants.LocalShell},
	} {
		action, err := parseAction(g.action)
		if err != nil {
			return nil, fmt.Errorf("agentdecl: posture %q: %w", ir.Posture, err)
		}
		rules = append(rules, hitlservice.Rule{Tools: g.tools, Tool: g.tool, Action: action})
	}

	defaultAction, err := parseAction(cfg.Policy.DefaultAction)
	if err != nil {
		return nil, fmt.Errorf("agentdecl: policy.default_action: %w", err)
	}

	return &hitlservice.Policy{
		Version:       hitlservice.PolicySchemaVersion,
		DefaultAction: defaultAction,
		Rules:         rules,
		Compute:       computeBounds(ir, cfg),
		Attention:     attentionBounds(ir, cfg),
	}, nil
}

// missionToolRules grants a subagent its own back-channel. These four are how
// an unattended run reports, plans, asks and finishes — they change nothing in
// the world, and gating them would park the unit on an approval nobody is there
// to answer, leaving it unable to say so. They sit after the standing denies, so
// an operator's credential rule still cannot be widened by them.
//
// Deliberately not granted: mission_start (a subagent may not spawn subagents —
// depth is exactly one) and the supervisor reads, which are unreachable off a
// firing session anyway.
func missionToolRules(ir *AgentIR) []hitlservice.Rule {
	if !ir.RunsAsSubagent() {
		return nil
	}
	names := []string{
		missiontools.ToolNameReport,
		missiontools.ToolNamePlan,
		missiontools.ToolNameAskAttention,
		missiontools.ToolNameFinish,
	}
	out := make([]hitlservice.Rule, 0, len(names))
	for _, name := range names {
		out = append(out, hitlservice.Rule{
			Tools:  missiontools.ToolsProviderName,
			Tool:   name,
			Action: hitlservice.ActionAllow,
		})
	}
	return out
}

// RunsAsSubagent reports whether the declaration asked to be entered as a
// mission unit. It decides the two halves of the envelope a subagent needs and
// a primary agent has no use for: a turn budget for the drive loop, and an
// attention block naming who may answer for it.
func (ir *AgentIR) RunsAsSubagent() bool {
	return ir.Role == RoleMission || ir.Role == RoleBoth
}

// computeBounds maps the source's turn cap onto both ceilings it can bound. A
// turn and a tool call are not the same unit — a turn may carry several calls —
// so the tool-call mapping only ever tightens, never widens, the shipped
// ceiling. MaxTurns is the drive loop's own budget and only means anything for
// a subagent, whose turns the runtime drives; a primary agent's turns are the
// operator's own prompts and are not the runtime's to cap.
func computeBounds(ir *AgentIR, cfg Config) *hitlservice.ComputeBounds {
	bounds := &hitlservice.ComputeBounds{
		MaxToolCalls: cfg.Policy.Compute.MaxToolCalls,
		MaxTokens:    cfg.Policy.Compute.MaxTokens,
		OnExhausted:  hitlservice.OnExhausted(cfg.Policy.Compute.OnExhausted),
	}
	declared := 0
	if ir.Budgets.MaxTurns != nil && *ir.Budgets.MaxTurns > 0 {
		declared = *ir.Budgets.MaxTurns
	}
	if declared > 0 && declared < bounds.MaxToolCalls {
		bounds.MaxToolCalls = declared
	}
	// Only 1 means anything: the drive loop issues at most two prompts, so a cap
	// of 1 drops the nudge and any larger number is already above the ceiling.
	// Emitting one anyway would read as enforced while doing nothing, and
	// hitlservice.VetPolicy refuses it outright.
	if ir.RunsAsSubagent() && (cfg.Policy.Compute.MaxTurns == 1 || declared == 1) {
		bounds.MaxTurns = 1
	}
	return bounds
}

// attentionBounds emits who besides a human may resolve this agent's asks.
// Only a subagent gets one: a primary agent is talking to the operator already,
// so it has nobody to escalate to and no budget to spend.
func attentionBounds(ir *AgentIR, cfg Config) *hitlservice.AttentionBounds {
	if !ir.RunsAsSubagent() {
		return nil
	}
	a := cfg.Policy.Attention
	if !a.AllowAgentAnswers && !a.AllowAgentApprovals {
		// The default stance. Emitting the all-false block would read as a
		// considered grant of nothing; absent says the same thing and matches
		// what an envelope with no attention block already means.
		return nil
	}
	return &hitlservice.AttentionBounds{
		AllowAgentAnswers:   a.AllowAgentAnswers,
		MaxAgentAnswers:     a.MaxAgentAnswers,
		AllowAgentApprovals: a.AllowAgentApprovals,
		MaxAgentApprovals:   a.MaxAgentApprovals,
	}
}

// standingRules renders the operator's non-waivable rules. Denies are appended
// before allows, so first-match-wins keeps a credential deny ahead of any grant.
func standingRules(in []StandingRule, action hitlservice.Action) []hitlservice.Rule {
	out := make([]hitlservice.Rule, 0, len(in))
	for _, r := range in {
		rule := hitlservice.Rule{Tools: r.Tools, Tool: r.Tool, Action: action}
		if r.WhenKey != "" {
			rule.When = []hitlservice.Condition{{
				Key:   r.WhenKey,
				Op:    hitlservice.ConditionOp(r.WhenOp),
				Value: r.WhenValue,
			}}
		}
		out = append(out, rule)
	}
	return out
}

func parseAction(s string) (hitlservice.Action, error) {
	switch hitlservice.Action(s) {
	case hitlservice.ActionAllow:
		return hitlservice.ActionAllow, nil
	case hitlservice.ActionApprove:
		return hitlservice.ActionApprove, nil
	case hitlservice.ActionDeny:
		return hitlservice.ActionDeny, nil
	default:
		return "", fmt.Errorf("unknown action %q (want allow, approve or deny)", s)
	}
}
