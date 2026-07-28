package libacp

import (
	"encoding/json"
	"fmt"
)

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HttpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type McpServerKind string

const (
	McpServerKindStdio McpServerKind = ""
	McpServerKindHTTP  McpServerKind = "http"
	McpServerKindSSE   McpServerKind = "sse"
)

type McpServer struct {
	Type    string          `json:"type,omitempty"`
	Name    string          `json:"name"`
	Command string          `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     []EnvVariable   `json:"env,omitempty"`
	URL     string          `json:"url,omitempty"`
	Headers []HttpHeader    `json:"headers,omitempty"`
	Meta    json.RawMessage `json:"_meta,omitempty"`
}

func (m McpServer) Kind() McpServerKind {
	switch m.Type {
	case "http":
		return McpServerKindHTTP
	case "sse":
		return McpServerKindSSE
	default:
		return McpServerKindStdio
	}
}

// mcpServerHttpWire and mcpServerStdioWire are McpServer's two wire shapes for
// MarshalJSON: unlike McpServer itself, args/env and headers have no
// omitempty, because the spec requires them always present (even as `[]`)
// for their respective transport.
type mcpServerHttpWire struct {
	Type    string          `json:"type,omitempty"`
	Name    string          `json:"name"`
	URL     string          `json:"url,omitempty"`
	Headers []HttpHeader    `json:"headers"`
	Meta    json.RawMessage `json:"_meta,omitempty"`
}

type mcpServerStdioWire struct {
	Name    string          `json:"name"`
	Command string          `json:"command,omitempty"`
	Args    []string        `json:"args"`
	Env     []EnvVariable   `json:"env"`
	Meta    json.RawMessage `json:"_meta,omitempty"`
}

// MarshalJSON forces args/env (stdio) and headers (http/sse) onto the wire as
// `[]` rather than omitting them when empty, since the spec declares them
// always-serialized with no default and omitempty can't express that on the
// flattened McpServer struct — hence the two per-transport wire shapes above.
func (m McpServer) MarshalJSON() ([]byte, error) {
	switch m.Kind() {
	case McpServerKindHTTP, McpServerKindSSE:
		headers := m.Headers
		if headers == nil {
			headers = []HttpHeader{}
		}
		return json.Marshal(mcpServerHttpWire{
			Type:    m.Type,
			Name:    m.Name,
			URL:     m.URL,
			Headers: headers,
			Meta:    m.Meta,
		})
	default:
		args, env := m.Args, m.Env
		if args == nil {
			args = []string{}
		}
		if env == nil {
			env = []EnvVariable{}
		}
		return json.Marshal(mcpServerStdioWire{
			Name:    m.Name,
			Command: m.Command,
			Args:    args,
			Env:     env,
			Meta:    m.Meta,
		})
	}
}

func (m McpServer) Validate() error {
	switch m.Kind() {
	case McpServerKindStdio:
		if m.Command == "" {
			return fmt.Errorf("libacp: stdio mcp server %q missing command", m.Name)
		}
	case McpServerKindHTTP, McpServerKindSSE:
		if m.URL == "" {
			return fmt.Errorf("libacp: %s mcp server %q missing url", m.Type, m.Name)
		}
	}
	return nil
}
