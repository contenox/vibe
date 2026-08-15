package agentdecl

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// ConfigFilename is the operator-editable overlay read from each root.
const ConfigFilename = "agents.toml"

//go:embed agents.toml
var shippedConfig []byte

// Config is agents.toml: everything an agent declaration cannot state, plus
// the names its tools and models resolve to.
type Config struct {
	Version       int                          `toml:"version"`
	Chain         ChainDefaults                `toml:"chain"`
	Routing       RoutingDefaults              `toml:"routing"`
	ToolsPolicies map[string]map[string]string `toml:"tools_policies"`
	Policy        PolicyDefaults               `toml:"policy"`
	Naming        NamingDefaults               `toml:"naming"`
	// Tools maps a name a declaration may use onto a connected tool,
	// "toolset.tool". Models maps a declaration's model name onto
	// "provider:model".
	Tools  map[string]string `toml:"tools"`
	Models map[string]string `toml:"models"`
	// Agents holds per-agent overlays keyed by the agent's id, each carrying
	// any subset of the sections above. Held raw so [For] can replay it through
	// the same "omitted keys keep the inherited value" decode the roots use,
	// which a typed struct cannot do: it could not tell `retry_on_failure = 0`
	// from an absent key.
	Agents map[string]map[string]any `toml:"agents"`
}

// ChainDefaults are the execution bounds no declaration states.
type ChainDefaults struct {
	TokenLimit int64  `toml:"token_limit"`
	MaxTokens  string `toml:"max_tokens"`
	Think      string `toml:"think"`
	// MainRounds and RecoveryRounds bound the two-stage tool loop.
	MainRounds     int `toml:"main_rounds"`
	RecoveryRounds int `toml:"recovery_rounds"`
	RetryOnFailure int `toml:"retry_on_failure"`
}

// RoutingDefaults hold model routing, as templates unless the operator pins them.
type RoutingDefaults struct {
	Model    string `toml:"model"`
	Provider string `toml:"provider"`
	// PinModel emits the source's own model instead of the templates, when the
	// registry could resolve it.
	PinModel bool `toml:"pin_model"`
}

// PolicyDefaults describe the emitted human-in-the-loop policy.
type PolicyDefaults struct {
	DefaultAction string          `toml:"default_action"`
	Compute       ComputeDefaults `toml:"compute"`
	// Attention is emitted only for a mission-role agent; a primary agent has
	// nobody to escalate to.
	Attention  AttentionDefaults `toml:"attention"`
	AlwaysDeny []StandingRule    `toml:"always_deny"`
	// AlwaysAllow grants tools the postures do not name — typically a tool the
	// operator connected. Emitted after AlwaysDeny, so it can never widen past
	// a credential deny.
	AlwaysAllow []StandingRule           `toml:"always_allow"`
	Postures    map[string]PostureGrants `toml:"postures"`
}

// ComputeDefaults bound what one run may spend.
type ComputeDefaults struct {
	MaxToolCalls int    `toml:"max_tool_calls"`
	MaxTokens    int    `toml:"max_tokens"`
	OnExhausted  string `toml:"on_exhausted"`
	// MaxTurns is the drive loop's own budget; it applies to a mission-role
	// agent only, since a primary agent's turns are the operator's prompts.
	MaxTurns int `toml:"max_turns"`
}

// AttentionDefaults say who besides a human may resolve a mission-role agent's
// asks. Both grants default off: an imported declaration states what an agent
// should do, never who may answer for it.
type AttentionDefaults struct {
	AllowAgentAnswers   bool `toml:"allow_agent_answers"`
	MaxAgentAnswers     int  `toml:"max_agent_answers"`
	AllowAgentApprovals bool `toml:"allow_agent_approvals"`
	MaxAgentApprovals   int  `toml:"max_agent_approvals"`
}

// StandingRule is a rule emitted under every posture, which a declaration can
// neither request nor waive.
type StandingRule struct {
	Tools     string `toml:"tools"`
	Tool      string `toml:"tool"`
	WhenKey   string `toml:"when_key"`
	WhenOp    string `toml:"when_op"`
	WhenValue string `toml:"when_value"`
}

// PostureGrants is how one permission setting widens into rules.
type PostureGrants struct {
	LocalFSRead  string `toml:"local_fs_read"`
	LocalFSWrite string `toml:"local_fs_write"`
	LocalShell   string `toml:"local_shell"`
}

// NamingDefaults control emitted identity.
type NamingDefaults struct {
	ScopeWithDialect bool `toml:"scope_with_dialect"`
}

// Shipped returns the configuration compiled into the binary, without operator
// overlays.
func Shipped() (Config, error) {
	var d Config
	if err := toml.Unmarshal(shippedConfig, &d); err != nil {
		return Config{}, fmt.Errorf("agentdecl: parse shipped config: %w", err)
	}
	return d, nil
}

// LoadDefaults returns the shipped defaults with each root's overlay applied in
// order, so a later root overrides an earlier one. Roots are passed
// weakest-first. A root without the file is skipped; an unreadable or malformed
// one is an error rather than a silent fall back to shipped values.
func Load(roots ...string) (Config, error) {
	d, err := Shipped()
	if err != nil {
		return Config{}, err
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		path := filepath.Join(root, ConfigFilename)
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Config{}, fmt.Errorf("agentdecl: read %s: %w", path, err)
		}
		tools, models, agents := d.Tools, d.Models, d.Agents
		if err := toml.Unmarshal(raw, &d); err != nil {
			return Config{}, fmt.Errorf("agentdecl: parse %s: %w", path, err)
		}
		// A decoded map replaces rather than merges, so naming one tool would
		// drop every shipped mapping.
		d.Tools = mergeNames(tools, d.Tools)
		d.Models = mergeNames(models, d.Models)
		d.Agents = mergeAgents(agents, d.Agents)
	}
	return d, nil
}

// For resolves the configuration one agent runs under: the root-wide values
// with that agent's [agents.<id>] overlay applied. An agent without a section
// gets the root-wide values unchanged.
//
// The overlay is replayed through toml.Unmarshal onto the already-resolved
// values, so it inherits the same precedence the roots get — a key the overlay
// omits keeps what it inherited, including zero values and false.
func (cfg Config) For(agent string) (Config, error) {
	overlay, ok := cfg.Agents[agent]
	if !ok || len(overlay) == 0 {
		// Cleared even here: what comes back is one agent's resolved
		// configuration, and other agents' overlays are not part of it.
		out := cfg
		out.Agents = nil
		return out, nil
	}
	raw, err := toml.Marshal(overlay)
	if err != nil {
		return Config{}, fmt.Errorf("agentdecl: re-encode [agents.%s]: %w", agent, err)
	}

	out := cfg
	toolsPolicies, postures := out.ToolsPolicies, out.Policy.Postures
	deny, allow := out.Policy.AlwaysDeny, out.Policy.AlwaysAllow
	tools, models := out.Tools, out.Models
	// Emptied so what survives the decode is exactly what the overlay declared,
	// and so the decode cannot reach the root's maps: it merges into an existing
	// map in place rather than replacing it, and every agent shares these.
	out.ToolsPolicies, out.Policy.Postures = nil, nil
	out.Policy.AlwaysDeny, out.Policy.AlwaysAllow = nil, nil
	out.Tools, out.Models = nil, nil
	out.Agents = nil

	if err := toml.Unmarshal(raw, &out); err != nil {
		return Config{}, fmt.Errorf("agentdecl: parse [agents.%s]: %w", agent, err)
	}

	out.ToolsPolicies = mergeToolsPolicies(toolsPolicies, out.ToolsPolicies)
	out.Policy.Postures = mergePostures(postures, out.Policy.Postures)
	// Standing rules append rather than replace, and the root's come first:
	// first match wins, so a per-agent grant can never reach past a credential
	// deny the root declared.
	out.Policy.AlwaysDeny = concatRules(deny, out.Policy.AlwaysDeny)
	out.Policy.AlwaysAllow = concatRules(allow, out.Policy.AlwaysAllow)
	// Copied before merging: mergeNames writes into its base, and the root's
	// tables outlive this agent.
	out.Tools = mergeNames(copyNames(tools), out.Tools)
	out.Models = mergeNames(copyNames(models), out.Models)

	if err := out.Validate(); err != nil {
		return Config{}, fmt.Errorf("agentdecl: [agents.%s]: %w", agent, err)
	}
	return out, nil
}

// UnknownAgents reports overlay sections naming an agent that does not exist.
// A mistyped id is otherwise a silent no-op, which reads as the knob having no
// effect rather than as a typo.
func (cfg Config) UnknownAgents(known map[string]bool) []string {
	var out []string
	for name := range cfg.Agents {
		if !known[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Validate reports defaults that would emit a chain that cannot run.
func (cfg Config) Validate() error {
	if cfg.Chain.TokenLimit <= 0 {
		return fmt.Errorf("agentdecl: chain.token_limit must be positive, got %d", cfg.Chain.TokenLimit)
	}
	if cfg.Chain.MainRounds <= 0 {
		return fmt.Errorf("agentdecl: chain.main_rounds must be positive, got %d", cfg.Chain.MainRounds)
	}
	if cfg.Chain.RecoveryRounds <= 0 {
		return fmt.Errorf("agentdecl: chain.recovery_rounds must be positive, got %d", cfg.Chain.RecoveryRounds)
	}
	for _, p := range []Posture{PostureReadOnly, PostureAskAlways, PostureAutoEdit} {
		if _, ok := cfg.Policy.Postures[string(p)]; !ok {
			return fmt.Errorf("agentdecl: policy.postures is missing %q", p)
		}
	}
	return nil
}

func mergeNames(base, overlay map[string]string) map[string]string {
	if base == nil {
		return overlay
	}
	for k, v := range overlay {
		base[k] = v
	}
	return base
}

// mergeToolsPolicies merges per-knob, not per-toolset: naming one shell knob
// must not drop the rest of the shell policy, nor the other toolsets.
func copyNames(base map[string]string) map[string]string {
	out := make(map[string]string, len(base))
	for k, v := range base {
		out[k] = v
	}
	return out
}

func mergeToolsPolicies(base, overlay map[string]map[string]string) map[string]map[string]string {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]map[string]string, len(base)+len(overlay))
	for set, knobs := range base {
		copied := make(map[string]string, len(knobs))
		for k, v := range knobs {
			copied[k] = v
		}
		out[set] = copied
	}
	for set, knobs := range overlay {
		if out[set] == nil {
			out[set] = make(map[string]string, len(knobs))
		}
		for k, v := range knobs {
			out[set][k] = v
		}
	}
	return out
}

// mergePostures merges per posture, so redefining one leaves the others as the
// root declared them. Validate still requires the full set to survive.
func mergePostures(base, overlay map[string]PostureGrants) map[string]PostureGrants {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]PostureGrants, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// mergeAgents merges overlay sections per agent, so a workspace naming one knob
// for one agent keeps what the home root said about the others.
func mergeAgents(base, overlay map[string]map[string]any) map[string]map[string]any {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]map[string]any, len(base)+len(overlay))
	for name, section := range base {
		out[name] = section
	}
	for name, section := range overlay {
		if out[name] == nil {
			out[name] = section
			continue
		}
		out[name] = mergeRaw(out[name], section)
	}
	return out
}

func mergeRaw(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		sub, ok := v.(map[string]any)
		if !ok {
			out[k] = v
			continue
		}
		prior, ok := out[k].(map[string]any)
		if !ok {
			out[k] = v
			continue
		}
		out[k] = mergeRaw(prior, sub)
	}
	return out
}

// concatRules copies rather than appending in place: the root's slice is shared
// by every agent resolved from the same Config.
func concatRules(root, agent []StandingRule) []StandingRule {
	if len(agent) == 0 {
		return root
	}
	out := make([]StandingRule, 0, len(root)+len(agent))
	out = append(out, root...)
	return append(out, agent...)
}
