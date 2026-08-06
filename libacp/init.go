package libacp

import "encoding/json"

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool            `json:"readTextFile,omitempty"`
	WriteTextFile bool            `json:"writeTextFile,omitempty"`
	Meta          json.RawMessage `json:"_meta,omitempty"`
}

// AuthCapabilities is the client-side auth capability object (unstable spec
// surface): it gates which auth method types the client can handle.
type AuthCapabilities struct {
	Terminal bool `json:"terminal,omitempty"`
}

// ClientSessionCapabilities mirrors the spec's clientCapabilities.session.
type ClientSessionCapabilities struct {
	ConfigOptions *SessionConfigOptionsCapabilities `json:"configOptions,omitempty"`
	Meta          json.RawMessage                   `json:"_meta,omitempty"`
}

type SessionConfigOptionsCapabilities struct {
	// Boolean present ({}) means the client accepts type:"boolean" config
	// options and may send boolean set_config_option values.
	Boolean *struct{}       `json:"boolean,omitempty"`
	Meta    json.RawMessage `json:"_meta,omitempty"`
}

type ClientCapabilities struct {
	FS       FileSystemCapabilities     `json:"fs,omitempty"`
	Terminal bool                       `json:"terminal,omitempty"`
	Session  *ClientSessionCapabilities `json:"session,omitempty"`
	Auth     AuthCapabilities           `json:"auth,omitempty"`
	Meta     json.RawMessage            `json:"_meta,omitempty"`
}

// SupportsBooleanConfigOptions reports whether the client advertised
// clientCapabilities.session.configOptions.boolean.
func (c ClientCapabilities) SupportsBooleanConfigOptions() bool {
	return c.Session != nil && c.Session.ConfigOptions != nil && c.Session.ConfigOptions.Boolean != nil
}

type PromptCapabilities struct {
	Image           bool            `json:"image,omitempty"`
	Audio           bool            `json:"audio,omitempty"`
	EmbeddedContext bool            `json:"embeddedContext,omitempty"`
	Meta            json.RawMessage `json:"_meta,omitempty"`
}

type McpCapabilities struct {
	HTTP bool            `json:"http,omitempty"`
	SSE  bool            `json:"sse,omitempty"`
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type SessionCapabilities struct {
	List   *struct{} `json:"list,omitempty"`
	Resume *struct{} `json:"resume,omitempty"`
	Close  *struct{} `json:"close,omitempty"`
	Delete *struct{} `json:"delete,omitempty"`
	// AdditionalDirectories present ({}) means the agent honors
	// additionalDirectories on session/new, session/load, and session/resume,
	// and may report SessionInfo.AdditionalDirectories from session/list.
	AdditionalDirectories *struct{}       `json:"additionalDirectories,omitempty"`
	Meta                  json.RawMessage `json:"_meta,omitempty"`
}

// AgentAuthCapabilities describes agent auth capabilities — currently just
// whether it supports the `logout` method.
type AgentAuthCapabilities struct {
	Logout *LogoutCapabilities `json:"logout,omitempty"`
	Meta   json.RawMessage     `json:"_meta,omitempty"`
}

// LogoutCapabilities is present ({}) when the agent supports the `logout`
// method.
type LogoutCapabilities struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type AgentCapabilities struct {
	LoadSession         bool                  `json:"loadSession,omitempty"`
	PromptCapabilities  PromptCapabilities    `json:"promptCapabilities,omitempty"`
	McpCapabilities     McpCapabilities       `json:"mcpCapabilities,omitempty"`
	SessionCapabilities SessionCapabilities   `json:"sessionCapabilities,omitempty"`
	Auth                AgentAuthCapabilities `json:"auth,omitempty"`
	Meta                json.RawMessage       `json:"_meta,omitempty"`
}

// AuthMethod covers the spec's auth method union. Type discriminates on the
// wire: "" (stable default), "terminal" (unstable; Args/Env launch the agent
// binary for a TUI), or "env_var" (unstable; Vars lists env vars to collect).
type AuthMethod struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Type        string            `json:"type,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Vars        []AuthEnvVar      `json:"vars,omitempty"`
	Link        string            `json:"link,omitempty"`
	Meta        json.RawMessage   `json:"_meta,omitempty"`
}

const (
	AuthMethodTypeTerminal = "terminal"
	AuthMethodTypeEnvVar   = "env_var"
)

// AuthEnvVar describes one variable of an env_var auth method. Secret is a
// pointer because the spec default is true: nil emits nothing (client assumes
// secret), an explicit false must reach the wire.
type AuthEnvVar struct {
	Name     string          `json:"name"`
	Label    string          `json:"label,omitempty"`
	Secret   *bool           `json:"secret,omitempty"`
	Optional bool            `json:"optional,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	Meta               json.RawMessage    `json:"_meta,omitempty"`
}

type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities,omitempty"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
	Meta              json.RawMessage   `json:"_meta,omitempty"`
}

type AuthenticateRequest struct {
	MethodID string          `json:"methodId"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type AuthenticateResponse struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// LogoutRequest terminates the current authenticated session. Only meaningful
// when the agent advertises AgentCapabilities.Auth.Logout.
type LogoutRequest struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type LogoutResponse struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}
