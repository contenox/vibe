package agentdecl

import (
	"errors"
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
// govern it. Rules are first-match-wins, so unconditional denies precede every
// grant, and an unnamed tool falls to DefaultAction.
func EmitPolicy(ir *AgentIR, cfg Config) (*hitlservice.Policy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if ir.Posture == PostureUnsafe {
		return nil, &ErrUnsafePosture{Name: ir.Name, Dialect: ir.Source.Dialect}
	}

	if _, err := parseAction(cfg.Policy.DefaultAction); err != nil {
		return nil, fmt.Errorf("agentdecl: policy.default_action: %w", err)
	}
	env, err := cfg.PostureEnvelope(ir.Posture)
	if err != nil {
		return nil, err
	}
	// The [policy] block is the root every posture sits in: its standing rules
	// are the ones a declaration can neither request nor waive, so they lead.
	env.AlwaysDeny = concatStandingRules(cfg.Policy.AlwaysDeny, env.AlwaysDeny)
	env.AlwaysAllow = concatStandingRules(cfg.Policy.AlwaysAllow, env.AlwaysAllow)
	if env.DefaultAction == "" {
		env.DefaultAction = cfg.Policy.DefaultAction
	}

	// The mission tools land where the missions axis would: after every standing
	// rule, before any capability grant.
	out, err := env.transpile(transpileOptions{extraRules: missionToolRules(ir)})
	if err != nil {
		return nil, err
	}
	policy := out.Policy
	// An envelope that states its own bounds keeps them; otherwise the [policy]
	// block's, narrowed by what the declaration asked for.
	if len(env.Compute) == 0 {
		policy.Compute = computeBounds(ir, cfg)
	}
	if len(env.Attention) == 0 && env.Axes[AxisMissionsAnswer].Grant == "" {
		policy.Attention = attentionBounds(ir, cfg)
	}
	return policy, nil
}

// PostureEnvelope resolves one posture to the envelope that expresses it. The
// envelopes table is authoritative; a configuration that still writes
// [policy.postures] directly is adapted onto the same axes, so both reach the
// emitter through one vocabulary.
func (cfg Config) PostureEnvelope(p Posture) (Envelope, error) {
	env, err := cfg.ResolveEnvelope(string(p))
	switch {
	case err == nil:
		return env, nil
	case !errors.Is(err, ErrNoEnvelope):
		return Envelope{}, err
	}
	grants, ok := cfg.Policy.Postures[string(p)]
	if !ok {
		return Envelope{}, fmt.Errorf("agentdecl: no posture grants configured for %q", p)
	}
	out := Envelope{Name: string(p), Axes: map[string]AxisGrant{}}
	for axis, action := range map[string]string{
		AxisFilesRead:  grants.LocalFSRead,
		AxisFilesWrite: grants.LocalFSWrite,
		AxisShell:      grants.LocalShell,
	} {
		if action == "" {
			continue
		}
		if _, err := parseAction(action); err != nil {
			return Envelope{}, fmt.Errorf("agentdecl: posture %q: %w", p, err)
		}
		out.Axes[axis] = AxisGrant{Grant: action}
	}
	return out, nil
}

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

// RunsAsSubagent reports whether the declaration asked to be entered as a mission
// unit, which decides the turn budget and attention block it gets.
func (ir *AgentIR) RunsAsSubagent() bool {
	return ir.Role == RoleMission || ir.Role == RoleBoth
}

func computeBounds(ir *AgentIR, cfg Config) *hitlservice.ComputeBounds {
	bounds := &hitlservice.ComputeBounds{
		MaxToolCalls:     cfg.Policy.Compute.MaxToolCalls,
		MaxTokens:        cfg.Policy.Compute.MaxTokens,
		OnExhausted:      hitlservice.OnExhausted(cfg.Policy.Compute.OnExhausted),
		ModelAllowlist:   cfg.Policy.Compute.ModelAllowlist,
		BackendAllowlist: cfg.Policy.Compute.BackendAllowlist,
	}
	declared := 0
	if ir.Budgets.MaxTurns != nil && *ir.Budgets.MaxTurns > 0 {
		declared = *ir.Budgets.MaxTurns
	}
	if declared > 0 && declared < bounds.MaxToolCalls {
		bounds.MaxToolCalls = declared
	}
	// Only 1 means anything: the drive loop issues at most two prompts, so any
	// larger number is already above the ceiling.
	if ir.RunsAsSubagent() && (cfg.Policy.Compute.MaxTurns == 1 || declared == 1) {
		bounds.MaxTurns = 1
	}
	return bounds
}

func attentionBounds(ir *AgentIR, cfg Config) *hitlservice.AttentionBounds {
	if !ir.RunsAsSubagent() {
		return nil
	}
	a := cfg.Policy.Attention
	if !a.AllowAgentAnswers && !a.AllowAgentApprovals {
		return nil
	}
	return &hitlservice.AttentionBounds{
		AllowAgentAnswers:   a.AllowAgentAnswers,
		MaxAgentAnswers:     a.MaxAgentAnswers,
		AllowAgentApprovals: a.AllowAgentApprovals,
		MaxAgentApprovals:   a.MaxAgentApprovals,
	}
}

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
