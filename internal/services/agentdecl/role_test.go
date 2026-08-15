package agentdecl_test

import (
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/stretchr/testify/require"
)

func roleConfig(t *testing.T) agentdecl.Config {
	t.Helper()
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	return cfg
}

func roleIR(role agentdecl.Role) *agentdecl.AgentIR {
	return &agentdecl.AgentIR{
		Name:         "worker",
		Description:  "does one thing",
		SystemPrompt: "do the thing",
		Role:         role,
		Posture:      agentdecl.PostureAskAlways,
		Tools:        agentdecl.Tools{Inherit: true},
	}
}

// TestUnit_Role_DecidesTheTurnBudget pins the half of the envelope only a
// subagent can use: the drive loop's turn budget. A primary agent's turns are
// the operator's own prompts, so capping them would cut the operator off.
//
// Only 1 is emitted at all — the drive loop issues at most two prompts, so
// that is the only value with an effect, and hitlservice.VetPolicy refuses any
// other. This test asserted the shipped default was emitted verbatim without
// ever vetting the result, which is how a default of 24 shipped and made every
// declared agent fail `contenox vet`.
func TestUnit_Role_DecidesTheTurnBudget(t *testing.T) {
	cfg := roleConfig(t)
	require.Zero(t, cfg.Policy.Compute.MaxTurns,
		"the shipped default keeps the runtime's nudge; only an operator asks for 1")

	cfg.Policy.Compute.MaxTurns = 1

	sub, err := agentdecl.EmitPolicy(roleIR(agentdecl.RoleMission), cfg)
	require.NoError(t, err)
	require.NotNil(t, sub.Compute)
	require.Equal(t, 1, sub.Compute.MaxTurns,
		"a mission-role declaration must carry the drive loop's turn budget")

	primary, err := agentdecl.EmitPolicy(roleIR(agentdecl.RolePrimary), cfg)
	require.NoError(t, err)
	require.NotNil(t, primary.Compute)
	require.Zero(t, primary.Compute.MaxTurns,
		"a primary agent's turns are the operator's prompts; the runtime does not cap them")

	both, err := agentdecl.EmitPolicy(roleIR(agentdecl.RoleBoth), cfg)
	require.NoError(t, err)
	require.Equal(t, 1, both.Compute.MaxTurns,
		"an agent that may run either way still needs the budget for the way that has one")
}

// TestUnit_Role_DeclaredMaxTurnsOnlyTightens pins the direction of every
// declared budget: a source may narrow what the operator allowed, never widen it.
func TestUnit_Role_DeclaredMaxTurnsOnlyTightens(t *testing.T) {
	cfg := roleConfig(t)

	tighter := 3
	ir := roleIR(agentdecl.RoleMission)
	ir.Budgets.MaxTurns = &tighter
	got, err := agentdecl.EmitPolicy(ir, cfg)
	require.NoError(t, err)
	require.Equal(t, tighter, got.Compute.MaxToolCalls, "a smaller declared budget tightens the tool-call ceiling")
	require.Zero(t, got.Compute.MaxTurns,
		"3 is already above the drive loop's ceiling of 2, so no turn cap is emitted")

	one := 1
	ir = roleIR(agentdecl.RoleMission)
	ir.Budgets.MaxTurns = &one
	got, err = agentdecl.EmitPolicy(ir, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, got.Compute.MaxTurns, "a declaration asking for exactly one turn drops the nudge")

	wider := cfg.Policy.Compute.MaxToolCalls + 1000
	ir = roleIR(agentdecl.RoleMission)
	ir.Budgets.MaxTurns = &wider
	got, err = agentdecl.EmitPolicy(ir, cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.Policy.Compute.MaxToolCalls, got.Compute.MaxToolCalls,
		"a file authored elsewhere must not raise the operator's ceiling")
	require.Zero(t, got.Compute.MaxTurns)
}

// TestUnit_Role_AttentionIsSubagentOnlyAndOffByDefault pins who may answer for
// an imported agent: nobody but a human, until the operator says otherwise in
// their own config — and never at all for a primary agent, which has nobody to
// escalate to.
func TestUnit_Role_AttentionIsSubagentOnlyAndOffByDefault(t *testing.T) {
	cfg := roleConfig(t)
	require.False(t, cfg.Policy.Attention.AllowAgentAnswers, "the shipped stance is human-only")
	require.False(t, cfg.Policy.Attention.AllowAgentApprovals, "…on both halves")

	sub, err := agentdecl.EmitPolicy(roleIR(agentdecl.RoleMission), cfg)
	require.NoError(t, err)
	require.Nil(t, sub.Attention, "an all-false block would read as a considered grant; absent says the same thing")

	granted := cfg
	granted.Policy.Attention.AllowAgentAnswers = true
	granted.Policy.Attention.AllowAgentApprovals = true
	sub, err = agentdecl.EmitPolicy(roleIR(agentdecl.RoleMission), granted)
	require.NoError(t, err)
	require.NotNil(t, sub.Attention, "an operator who grants it gets it emitted")
	require.True(t, sub.Attention.AllowAgentAnswers)
	require.True(t, sub.Attention.AllowAgentApprovals)
	require.Equal(t, granted.Policy.Attention.MaxAgentAnswers, sub.Attention.MaxAgentAnswers)
	require.Equal(t, granted.Policy.Attention.MaxAgentApprovals, sub.Attention.MaxAgentApprovals)

	primary, err := agentdecl.EmitPolicy(roleIR(agentdecl.RolePrimary), granted)
	require.NoError(t, err)
	require.Nil(t, primary.Attention,
		"a primary agent talks to the operator; there is nobody for an oracle to answer on its behalf")
}

// TestUnit_Role_ClaudeCodeDeclarationsAreSubagents pins the dialect claim:
// .claude/agents/*.md IS the subagent format, so what it emits must be usable
// as one.
func TestUnit_Role_ClaudeCodeDeclarationsAreSubagents(t *testing.T) {
	ir := roleIR(agentdecl.RoleMission)
	ir.Source.Dialect = agentdecl.DialectClaudeCode
	require.True(t, ir.RunsAsSubagent())
	require.Equal(t, agentdecl.RoleMission, agentdecl.RoleSubagent,
		"the alias and the role must stay the same value; two names for one state is the bug, two states is worse")
}

// TestUnit_Preseed_NamesTheRealConfigFile pins the preseeded prose to
// ConfigFilename. The docs pointed at a name the loader never reads, which
// sends an operator to edit a file nothing loads.
func TestUnit_Preseed_NamesTheRealConfigFile(t *testing.T) {
	for _, f := range agentdecl.Preseeded {
		body := f.Content()
		require.NotContainsf(t, body, "import-defaults.toml",
			"%s names a config file the loader does not read", f.RelPath)
	}
}

// TestUnit_Preseed_ShipsASubagentExample pins that the authoring convention
// includes a worked subagent, since that is what /plan actually dispatches.
func TestUnit_Preseed_ShipsASubagentExample(t *testing.T) {
	var researcher string
	for _, f := range agentdecl.Preseeded {
		if filepath.Base(f.RelPath) == "researcher.md" {
			researcher = f.Content()
		}
	}
	require.NotEmpty(t, researcher, "the preseeded set must carry a subagent example")
	for _, tool := range []string{"mission_plan", "mission_report", "mission_ask_attention", "mission_finish"} {
		require.Containsf(t, researcher, tool, "a subagent example must name %s, its only channel out", tool)
	}
}

// TestUnit_Role_SubagentCanReachItsBackChannel is the one that would have
// caught the whole class: a declaration that NAMES its tools must still emit a
// subagent that can report, plan, ask and finish. Without the mission toolset a
// subagent runs, answers in prose nobody reads, and the drive loop files
// "ended two turns without reporting" — it looks like a model failure and is a
// transpiler bug.
func TestUnit_Role_SubagentCanReachItsBackChannel(t *testing.T) {
	cfg := roleConfig(t)
	allow, _, err := cfg.MapTools([]string{"Read", "Glob", "Grep"})
	require.NoError(t, err)

	ir := roleIR(agentdecl.RoleMission)
	ir.Tools = agentdecl.Tools{Allow: allow}

	chain, err := agentdecl.EmitChain(ir, cfg)
	require.NoError(t, err)
	require.Contains(t, chain.Tasks[0].ExecuteConfig.Tools, "mission",
		"a subagent that named its tools still holds its only channel out")
	require.Contains(t, chain.Tasks[0].ExecuteConfig.Tools, "local_fs",
		"…without losing the tools it did name")

	// Exposing the toolset is half of it: the envelope must not gate the
	// back-channel behind an approval nobody is there to answer.
	pol, err := agentdecl.EmitPolicy(ir, cfg)
	require.NoError(t, err)
	action := map[string]string{}
	for _, r := range pol.Rules {
		if r.Tools == "mission" {
			action[r.Tool] = string(r.Action)
		}
	}
	for _, tool := range []string{"mission_report", "mission_plan", "mission_ask_attention", "mission_finish"} {
		require.Equalf(t, "allow", action[tool],
			"%s must be allowed outright: it changes nothing in the world, and gating it parks the subagent on an ask it cannot report", tool)
	}
	require.NotContains(t, action, "mission_start",
		"a subagent may not spawn subagents; depth is exactly one")
}

// TestUnit_Role_PrimaryAgentGetsNoMissionGrant pins the other side: the grant
// is the role's, not everyone's.
func TestUnit_Role_PrimaryAgentGetsNoMissionGrant(t *testing.T) {
	cfg := roleConfig(t)
	allow, _, err := cfg.MapTools([]string{"Read"})
	require.NoError(t, err)

	ir := roleIR(agentdecl.RolePrimary)
	ir.Tools = agentdecl.Tools{Allow: allow}

	chain, err := agentdecl.EmitChain(ir, cfg)
	require.NoError(t, err)
	require.NotContains(t, chain.Tasks[0].ExecuteConfig.Tools, "mission")

	pol, err := agentdecl.EmitPolicy(ir, cfg)
	require.NoError(t, err)
	for _, r := range pol.Rules {
		require.NotEqual(t, "mission", r.Tools, "a primary agent is on no mission and needs no grant for one")
	}
}

// TestUnit_Role_StandingDeniesStayAheadOfTheMissionGrant pins ordering under
// first-match-wins: the operator's non-waivable denies are still evaluated
// before anything the role grants.
func TestUnit_Role_StandingDeniesStayAheadOfTheMissionGrant(t *testing.T) {
	cfg := roleConfig(t)
	require.NotEmpty(t, cfg.Policy.AlwaysDeny, "the shipped config must carry standing denies for this to mean anything")

	pol, err := agentdecl.EmitPolicy(roleIR(agentdecl.RoleMission), cfg)
	require.NoError(t, err)

	firstMission, lastDeny := -1, -1
	for i, r := range pol.Rules {
		if r.Tools == "mission" && firstMission < 0 {
			firstMission = i
		}
		if r.Action == "deny" && r.Tools != "mission" {
			lastDeny = i
		}
	}
	require.GreaterOrEqual(t, firstMission, 0)
	require.Less(t, lastDeny, firstMission,
		"a standing deny must be reachable before the role's grant, or the grant could widen it")
}
