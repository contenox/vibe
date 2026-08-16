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

// EmitChain renders one agent as a task chain, so a declared agent rides the
// same tool loop and retry behaviour as a native one. It emits one loop; see
// EmitTree for a directory of declarations that becomes a router over several.
func EmitChain(ir *AgentIR, cfg Config) (*taskengine.TaskChainDefinition, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := refuseUnsafe(ir); err != nil {
		return nil, err
	}

	id := ir.ScopedName(cfg.Naming.ScopeWithDialect)
	summarise := id + "-summarise"
	tasks := leafLoop(ir, ir, cfg, id, summarise)
	tasks = append(tasks, terminalTask(summarise, cfg, id+"-recovery"))

	return &taskengine.TaskChainDefinition{
		ID:          id,
		Description: ir.Description,
		TokenLimit:  cfg.Chain.TokenLimit,
		Tasks:       tasks,
	}, nil
}

// Loop macros a declaration may use. Both the round count and the budget are
// known only to the emitter, so a prompt cannot state either literally without
// going stale.
const (
	MacroRoundsUsed         = "{{rounds_used}}"
	MacroRecoveryRoundsUsed = "{{recovery_rounds_used}}"
	MacroMainRounds         = "{{main_rounds}}"
	MacroRecoveryRounds     = "{{recovery_rounds}}"
)

func expandLoopMacros(prompt, id string, cfg Config) string {
	if prompt == "" {
		return prompt
	}
	r := strings.NewReplacer(
		MacroRoundsUsed, "{{edge_count:"+id+"-agent->"+id+"-tools}}",
		MacroRecoveryRoundsUsed, "{{edge_count:"+id+"-recovery->"+id+"-recovery-tools}}",
		MacroMainRounds, strconv.Itoa(cfg.Chain.MainRounds),
		MacroRecoveryRounds, strconv.Itoa(cfg.Chain.RecoveryRounds),
	)
	return r.Replace(prompt)
}

func refuseUnsafe(ir *AgentIR) error {
	if ir.Posture != PostureUnsafe {
		return nil
	}
	return fmt.Errorf("agentdecl: %s asks for permissionMode: bypassPermissions — to skip every approval. "+
		"contenox will not run an agent that way. Use acceptEdits and grant what it actually needs "+
		"under [policy.postures] or [[policy.always_allow]] in agents.toml, where the grant is written down", ir.Name)
}

func leafLoop(ir, recoveryIR *AgentIR, cfg Config, id, terminalID string) []taskengine.TaskDefinition {
	agent := id + "-agent"
	tools := id + "-tools"
	recovery := id + "-recovery"

	exec := execConfig(ir, cfg, id, true)
	toolExec := execConfig(ir, cfg, id, false)
	prompt := expandLoopMacros(systemInstruction(ir), id, cfg)

	// With no recovery declaration an exhausted or failed turn goes straight to
	// the terminal.
	exhausted := recovery
	if recoveryIR == nil {
		exhausted = terminalID
	}

	out := []taskengine.TaskDefinition{
		{
			ID:                agent,
			Description:       "Imported agent turn. Calls tools or answers directly.",
			Handler:           taskengine.HandleChatCompletion,
			SystemInstruction: prompt,
			ExecuteConfig:     exec,
			RetryOnFailure:    cfg.Chain.RetryOnFailure,
			Transition: taskengine.TaskTransition{
				OnFailure: exhausted,
				Branches: []taskengine.TransitionBranch{
					{
						Operator: taskengine.OpEdgeTraversedAtLeast,
						Edge:     agent + "->" + tools,
						When:     strconv.Itoa(cfg.Chain.MainRounds),
						Goto:     exhausted,
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
	}
	if recoveryIR == nil {
		return out
	}
	return append(out, recoveryPair(recoveryIR, cfg, id, terminalID)...)
}

func recoveryPair(ir *AgentIR, cfg Config, id, terminalID string) []taskengine.TaskDefinition {
	agent := id + "-agent"
	recovery := id + "-recovery"
	recoveryTools := id + "-recovery-tools"
	return []taskengine.TaskDefinition{
		{
			ID:                recovery,
			Description:       "Bounded second attempt after the main stage exhausted its rounds or failed.",
			Handler:           taskengine.HandleChatCompletion,
			SystemInstruction: expandLoopMacros(systemInstruction(ir), id, cfg),
			ExecuteConfig:     withAltRouting(execConfig(ir, cfg, id, true), cfg),
			InputVar:          agent,
			Transition: taskengine.TaskTransition{
				OnFailure: terminalID,
				Branches: []taskengine.TransitionBranch{
					{
						Operator: taskengine.OpEdgeTraversedAtLeast,
						Edge:     recovery + "->" + recoveryTools,
						When:     strconv.Itoa(cfg.Chain.RecoveryRounds),
						Goto:     terminalID,
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
			ExecuteConfig: execConfig(ir, cfg, id, false),
			InputVar:      recovery,
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{
					{Operator: taskengine.OpDefault, When: "", Goto: recovery},
				},
			},
		},
	}
}

func terminalTask(id string, cfg Config, inputVar string) taskengine.TaskDefinition {
	return taskengine.TaskDefinition{
		ID:                id,
		Description:       "States what was attempted and why it stopped.",
		Handler:           taskengine.HandleChatCompletion,
		SystemInstruction: "Current date: {{date}}.\n\nReport what was attempted and why it could not be completed. Be specific and brief; do not retry.",
		ExecuteConfig:     summariseConfig(cfg),
		InputVar:          inputVar,
		Transition: taskengine.TaskTransition{
			Branches: []taskengine.TransitionBranch{
				{Operator: taskengine.OpDefault, When: "", Goto: taskengine.TermEnd},
			},
		},
	}
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

// A subagent additionally always holds the mission toolset: it is the only channel out of an
// unattended run, and it is inert off a mission.
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
		Model:             altModel(cfg),
		Provider:          altProvider(cfg),
		Think:             cfg.Chain.Think,
		MaxTokensTemplate: cfg.Chain.MaxTokens,
		PassClientsTools:  false,
	}
}

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

func systemInstruction(ir *AgentIR) string {
	b := strings.Builder{}
	b.WriteString(ir.SystemPrompt)
	if ir.Tools.Inherit || len(ir.Tools.Allow) > 0 {
		b.WriteString("\n\nAvailable tools (tools -> function names):\n{{tools}}")
	}
	b.WriteString("\n\nHost: os={{host:os}} arch={{host:arch}}")
	return b.String()
}

func altModel(cfg Config) string {
	if cfg.Routing.AltModel != "" {
		return cfg.Routing.AltModel
	}
	return cfg.Routing.Model
}

func altProvider(cfg Config) string {
	if cfg.Routing.AltProvider != "" {
		return cfg.Routing.AltProvider
	}
	return cfg.Routing.Provider
}

func withAltRouting(ec *taskengine.LLMExecutionConfig, cfg Config) *taskengine.LLMExecutionConfig {
	if ec == nil || cfg.Routing.PinModel {
		return ec
	}
	ec.Model = altModel(cfg)
	ec.Provider = altProvider(cfg)
	return ec
}
