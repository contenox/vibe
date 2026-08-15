// Package agentdecl reads agent declarations — Markdown with a YAML frontmatter
// header — and renders each as the task chain and human-in-the-loop policy that
// run it.
package agentdecl

import "github.com/contenox/contenox/internal/store/runtimetypes"

// Dialect names a source format.
type Dialect string

const (
	DialectClaudeCode  Dialect = "claude-code"
	DialectAntigravity Dialect = "antigravity"
	DialectCopilot     Dialect = "copilot"
	DialectOpenCode    Dialect = "opencode"
	DialectCursor      Dialect = "cursor"
)

// Source is the provenance of one parsed agent. SHA256 covers the source file
// verbatim; a re-import compares against it to tell an unchanged source from an
// edited one.
type Source struct {
	Dialect Dialect `json:"dialect"`
	// SpecDate is the documented spec version the dialect parser targets.
	SpecDate string `json:"spec_date"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
}

// Model is the requested model. Raw survives mapping so a later import can
// remap it without re-reading the source.
type Model struct {
	Raw      string `json:"raw"`
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
}

// Tools is the resolved grant in canonical contenox names ("local_fs.read_file").
// Inherit is set when the declaration named no tools at all, which the format
// reads as every tool rather than none.
type Tools struct {
	Allow   []string `json:"allow,omitempty"`
	Deny    []string `json:"deny,omitempty"`
	Inherit bool     `json:"inherit,omitempty"`
}

// Role is how the source expects the agent to be entered.
type Role string

const (
	RolePrimary Role = "primary"
	RoleMission Role = "mission"
	RoleBoth    Role = "both"
)

// RoleSubagent is what the source formats call [RoleMission].
const RoleSubagent = RoleMission

// Posture is the normalized permission intent, ordered most to least restrictive.
type Posture string

const (
	PostureReadOnly  Posture = "read_only"
	PostureAskAlways Posture = "ask_always"
	PostureAutoEdit  Posture = "auto_edit"
	// PostureUnsafe is a source asking for permission prompts to be skipped
	// entirely. Emitting it requires explicit operator consent.
	PostureUnsafe Posture = "unsafe"
)

// Budgets are execution bounds the source expressed. They bound what an agent
// may spend, so they land in the policy rather than the chain.
type Budgets struct {
	MaxTurns *int `json:"max_turns,omitempty"`
}

// Unmapped is one thing the source stated that contenox will not carry.
type Unmapped struct {
	Field  string `json:"field"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason"`
}

// DeclaredMCPServer is an MCP server an agent brought with it, rather than one
// the operator registered. Fields mirror runtimetypes.MCPServer; the row is
// built in the surface layer, which is the only place with a database.
type DeclaredMCPServer struct {
	// Declared is the name written in the declaration. The registered name is
	// derived from it and the agent id, so it is scoped rather than global.
	Declared  string            `json:"declared"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// AuthEnvKey names an environment variable holding the token. A literal
	// token is refused: a declaration is a file that gets committed.
	AuthEnvKey string `json:"auth_env_key,omitempty"`
	AuthType   string `json:"auth_type,omitempty"`
}

// DeclaredRemoteTool is an OpenAPI service an agent brought with it. Every
// operation in the spec becomes a callable tool, exactly as with
// `contenox tools add`.
type DeclaredRemoteTool struct {
	Declared    string            `json:"declared"`
	EndpointURL string            `json:"endpoint_url"`
	SpecURL     string            `json:"spec_url,omitempty"`
	TimeoutMs   int               `json:"timeout_ms,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// AgentIR is one parsed agent in the shape both emitters consume.
type AgentIR struct {
	Source       Source `json:"source"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt"`
	Model        Model  `json:"model"`
	Tools        Tools  `json:"tools"`
	// MCPServers are names of servers the operator already registered — a
	// grant, not a definition. DeclaredMCP and DeclaredRemote are definitions
	// the agent brought itself, registered scoped to it.
	MCPServers     []string             `json:"mcp_servers,omitempty"`
	DeclaredMCP    []DeclaredMCPServer  `json:"declared_mcp,omitempty"`
	DeclaredRemote []DeclaredRemoteTool `json:"declared_remote,omitempty"`
	Role           Role                 `json:"role"`
	Posture        Posture              `json:"posture"`
	// Think maps onto execute_config.think; the source and contenox
	// vocabularies for reasoning effort coincide.
	Think       string     `json:"think,omitempty"`
	Temperature *float32   `json:"temperature,omitempty"`
	Budgets     Budgets    `json:"budgets"`
	Unmapped    []Unmapped `json:"unmapped,omitempty"`
}

// DeclaredToolsetNames are the registered names of the sources this agent
// brought, in declaration order. The emitted chain lists them in
// execute_config.tools and the registrar writes rows under the same names —
// this function is the single place that contract is expressed.
func (ir *AgentIR) DeclaredToolsetNames(agentID string) []string {
	names := make([]string, 0, len(ir.DeclaredMCP)+len(ir.DeclaredRemote))
	for _, srv := range ir.DeclaredMCP {
		names = append(names, runtimetypes.DeclaredToolName(agentID, srv.Declared))
	}
	for _, tool := range ir.DeclaredRemote {
		names = append(names, runtimetypes.DeclaredToolName(agentID, tool.Declared))
	}
	return names
}

// AddUnmapped records a source field that will not be carried.
func (ir *AgentIR) AddUnmapped(field, value, reason string) {
	ir.Unmapped = append(ir.Unmapped, Unmapped{Field: field, Value: value, Reason: reason})
}

// ScopedName is the emitted chain id, which chainagents also uses as the
// registry name — so it is what an operator types at `mission fire` and reads
// in `agent list`. A declaration's own name carries through unchanged; the
// word chain belongs to the file this becomes, not to the agent.
//
// Scoping by dialect is what stops two products' identically named agents from
// resolving to one row, where the loser is dropped silently.
func (ir *AgentIR) ScopedName(scopeWithDialect bool) string {
	if !scopeWithDialect {
		return ir.Name
	}
	return string(ir.Source.Dialect) + "-" + ir.Name
}
