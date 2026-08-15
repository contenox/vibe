package agentdecl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/missiontools"
)

// ChainSchemaURL is stamped on emitted chains so an editor completes and
// validates them the way it does the shipped ones.
const ChainSchemaURL = "https://contenox.com/schema/task-chain.schema.json"

// EmitChain renders an agent as a task chain, parameterizing the shipped
// chain-run topology so a declared agent rides the same tool loop, retry
// behaviour and test coverage as a native one. No declaration format can
// express branching, a per-step model or a recovery path, so none is invented.
func EmitChain(ir *AgentIR, cfg Config) (*taskengine.TaskChainDefinition, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if ir.Posture == PostureUnsafe {
		return nil, fmt.Errorf("agentdecl: %s asks for permissionMode: bypassPermissions — to skip every approval. "+
			"contenox will not run an agent that way. Use acceptEdits and grant what it actually needs "+
			"under [policy.postures] or [[policy.always_allow]] in agents.toml, where the grant is written down", ir.Name)
	}

	id := ir.ScopedName(cfg.Naming.ScopeWithDialect)
	agent := id + "-agent"
	tools := id + "-tools"
	recovery := id + "-recovery"
	recoveryTools := id + "-recovery-tools"
	summarise := id + "-summarise"

	exec := execConfig(ir, cfg, id, true)
	toolExec := execConfig(ir, cfg, id, false)
	prompt := systemInstruction(ir)

	return &taskengine.TaskChainDefinition{
		ID:          id,
		Description: ir.Description,
		TokenLimit:  cfg.Chain.TokenLimit,
		Tasks: []taskengine.TaskDefinition{
			{
				ID:                agent,
				Description:       "Imported agent turn. Calls tools or answers directly.",
				Handler:           taskengine.HandleChatCompletion,
				SystemInstruction: prompt,
				ExecuteConfig:     exec,
				RetryOnFailure:    cfg.Chain.RetryOnFailure,
				Transition: taskengine.TaskTransition{
					OnFailure: recovery,
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpEdgeTraversedAtLeast,
							Edge:     agent + "->" + tools,
							When:     strconv.Itoa(cfg.Chain.MainRounds),
							Goto:     recovery,
						},
						{Operator: taskengine.OpEquals, When: "tool_call", Goto: tools},
						{Operator: taskengine.OpDefault, When: "", Goto: taskengine.TermEnd},
					},
				},
			},
			{
				ID:            tools,
				Description:   "Runs the tool calls the agent turn requested.",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: toolExec,
				InputVar:      agent,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, When: "", Goto: agent},
					},
				},
			},
			{
				ID:                recovery,
				Description:       "Bounded second attempt after the main stage exhausted its rounds or failed.",
				Handler:           taskengine.HandleChatCompletion,
				SystemInstruction: prompt,
				ExecuteConfig:     exec,
				InputVar:          agent,
				Transition: taskengine.TaskTransition{
					OnFailure: summarise,
					Branches: []taskengine.TransitionBranch{
						{
							Operator: taskengine.OpEdgeTraversedAtLeast,
							Edge:     recovery + "->" + recoveryTools,
							When:     strconv.Itoa(cfg.Chain.RecoveryRounds),
							Goto:     summarise,
						},
						{Operator: taskengine.OpEquals, When: "tool_call", Goto: recoveryTools},
						{Operator: taskengine.OpDefault, When: "", Goto: taskengine.TermEnd},
					},
				},
			},
			{
				ID:            recoveryTools,
				Description:   "Runs the tool calls the recovery turn requested.",
				Handler:       taskengine.HandleExecuteToolCalls,
				ExecuteConfig: toolExec,
				InputVar:      recovery,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, When: "", Goto: recovery},
					},
				},
			},
			{
				ID:                summarise,
				Description:       "States what was attempted and why it stopped.",
				Handler:           taskengine.HandleChatCompletion,
				SystemInstruction: "Report what was attempted and why it could not be completed. Be specific and brief; do not retry.",
				ExecuteConfig:     summariseConfig(cfg),
				InputVar:          recovery,
				Transition: taskengine.TaskTransition{
					Branches: []taskengine.TransitionBranch{
						{Operator: taskengine.OpDefault, When: "", Goto: taskengine.TermEnd},
					},
				},
			},
		},
	}, nil
}

func execConfig(ir *AgentIR, cfg Config, agentID string, withPrompt bool) *taskengine.LLMExecutionConfig {
	ec := &taskengine.LLMExecutionConfig{
		Tools:            exposedToolSets(ir, agentID),
		HideTools:        ir.Tools.Deny,
		ToolsPolicies:    toolsPoliciesFor(ir, cfg),
		PassClientsTools: false,
	}
	if withPrompt {
		ec.Model = cfg.Routing.Model
		ec.Provider = cfg.Routing.Provider
		if cfg.Routing.PinModel && ir.Model.ID != "" {
			ec.Model, ec.Provider = ir.Model.ID, ir.Model.Provider
		}
		ec.Think = cfg.Chain.Think
		if ir.Think != "" {
			ec.Think = ir.Think
		}
		ec.Temperature = ir.Temperature
		ec.MaxTokensTemplate = cfg.Chain.MaxTokens
	}
	return ec
}

// exposedToolSets is what execute_config.tools takes. A declaration that named
// no tools inherits every toolset, which the engine spells "*".
//
// A subagent additionally always holds the mission toolset. No declaration
// format has a word for it, and it is not a capability the operator is granting:
// it is the only channel out of an unattended run. Without it the subagent works,
// answers in prose nobody reads, and the drive loop files "ended two turns
// without reporting". The toolset is inert off a mission, so this grants a
// primary agent nothing even when the same file declares both roles.
func exposedToolSets(ir *AgentIR, agentID string) []string {
	var sets []string
	if ir.Tools.Inherit {
		// "*" already reaches the mission toolset and every server the
		// operator registered, so neither is restated here.
		sets = []string{"*"}
	} else {
		sets = ToolSets(ir.Tools.Allow)
		if ir.RunsAsSubagent() {
			sets = appendToolSet(sets, missiontools.ToolsProviderName)
		}
		// Servers the operator registered that this agent is granted.
		for _, name := range ir.MCPServers {
			sets = appendToolSet(sets, name)
		}
	}
	// Sources the agent brought itself are named exactly under either form:
	// "*" deliberately skips declaration-scoped toolsets, so one agent's
	// private source is never handed to every other agent.
	for _, name := range ir.DeclaredToolsetNames(agentID) {
		sets = appendToolSet(sets, name)
	}
	return sets
}

func appendToolSet(sets []string, name string) []string {
	if name == "" {
		return sets
	}
	for _, s := range sets {
		if s == name {
			return sets
		}
	}
	return append(sets, name)
}

func summariseConfig(cfg Config) *taskengine.LLMExecutionConfig {
	return &taskengine.LLMExecutionConfig{
		Model:             cfg.Routing.Model,
		Provider:          cfg.Routing.Provider,
		Think:             cfg.Chain.Think,
		MaxTokensTemplate: cfg.Chain.MaxTokens,
		PassClientsTools:  false,
	}
}

// toolsPoliciesFor narrows the shipped per-toolset knobs to the toolsets this
// agent actually exposes, so an imported agent does not carry policy for tools
// it cannot reach.
func toolsPoliciesFor(ir *AgentIR, cfg Config) map[string]map[string]string {
	sets := ToolSets(ir.Tools.Allow)
	if ir.Tools.Inherit {
		sets = make([]string, 0, len(cfg.ToolsPolicies))
		for set := range cfg.ToolsPolicies {
			sets = append(sets, set)
		}
		sort.Strings(sets)
	}
	if len(sets) == 0 {
		return nil
	}
	out := map[string]map[string]string{}
	for _, set := range sets {
		if knobs, ok := cfg.ToolsPolicies[set]; ok {
			copied := make(map[string]string, len(knobs))
			for k, v := range knobs {
				copied[k] = v
			}
			out[set] = copied
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// systemInstruction is the source prompt plus the macros a chain must declare
// for itself: nothing is appended implicitly at execution time, so a generated
// chain states its own tool listing and host facts.
func systemInstruction(ir *AgentIR) string {
	b := strings.Builder{}
	b.WriteString(ir.SystemPrompt)
	if ir.Tools.Inherit || len(ir.Tools.Allow) > 0 {
		b.WriteString("\n\nAvailable tools (tools -> function names):\n{{tools}}")
	}
	b.WriteString("\n\nHost: os={{host:os}} arch={{host:arch}}")
	return b.String()
}
