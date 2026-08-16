package agentdecl

import (
	"fmt"
	"sort"
	"strings"
)

// FieldMCPServers is Claude Code's own key. A list of names is a grant against
// servers the operator registered; a map is a set of definitions the agent
// brings with it.
const FieldMCPServers = "mcpServers"

// FieldRemoteTools has no counterpart in any source dialect; it is contenox's own.
const FieldRemoteTools = "remoteTools"

func parseMCPServers(path string, fields map[string]any, ir *AgentIR) error {
	raw, ok := fields[FieldMCPServers]
	if !ok || raw == nil {
		return nil
	}
	if defs, isMap := asStringMap(raw); isMap {
		for _, name := range sortedKeys(defs) {
			srv, err := mcpServerFrom(path, name, defs[name])
			if err != nil {
				return err
			}
			ir.DeclaredMCP = append(ir.DeclaredMCP, srv)
		}
		return nil
	}
	ir.MCPServers = append(ir.MCPServers, listField(fields, FieldMCPServers)...)
	return nil
}

func mcpServerFrom(path, name string, raw any) (DeclaredMCPServer, error) {
	body, ok := asStringMap(raw)
	if !ok {
		return DeclaredMCPServer{}, fmt.Errorf("agentdecl: %s: %s.%s must be a mapping of server settings", path, FieldMCPServers, name)
	}
	srv := DeclaredMCPServer{
		Declared:   name,
		Command:    mapString(body, "command"),
		URL:        mapString(body, "url"),
		Headers:    mapStringMap(body, "headers"),
		AuthEnvKey: mapString(body, "authEnvKey"),
		AuthType:   mapString(body, "authType"),
	}
	for _, arg := range mapList(body, "args") {
		srv.Args = append(srv.Args, arg)
	}

	// A declaration is a file that gets committed: an env var name may live in
	// one, the token it resolves to may not.
	for _, secret := range []string{"authToken", "token", "password", "secret"} {
		if mapString(body, secret) != "" {
			return DeclaredMCPServer{}, fmt.Errorf(
				"agentdecl: %s: %s.%s sets %q — a declaration is committed to source control, so it may not carry a literal credential. Name an environment variable with authEnvKey instead",
				path, FieldMCPServers, name, secret)
		}
	}

	srv.Transport = strings.ToLower(strings.TrimSpace(firstNonEmpty(mapString(body, "transport"), mapString(body, "type"))))
	if srv.Transport == "" {
		if srv.Command != "" {
			srv.Transport = "stdio"
		} else {
			srv.Transport = "http"
		}
	}
	switch srv.Transport {
	case "stdio":
		if srv.Command == "" {
			return DeclaredMCPServer{}, fmt.Errorf("agentdecl: %s: %s.%s is stdio but names no command", path, FieldMCPServers, name)
		}
	case "http", "sse":
		if srv.URL == "" {
			return DeclaredMCPServer{}, fmt.Errorf("agentdecl: %s: %s.%s is %s but names no url", path, FieldMCPServers, name, srv.Transport)
		}
	default:
		return DeclaredMCPServer{}, fmt.Errorf("agentdecl: %s: %s.%s has unknown transport %q (stdio, http, sse)", path, FieldMCPServers, name, srv.Transport)
	}
	return srv, nil
}

func parseRemoteTools(path string, fields map[string]any, ir *AgentIR) error {
	raw, ok := fields[FieldRemoteTools]
	if !ok || raw == nil {
		return nil
	}
	defs, isMap := asStringMap(raw)
	if !isMap {
		return fmt.Errorf("agentdecl: %s: %s must be a mapping of tool name to settings", path, FieldRemoteTools)
	}
	for _, name := range sortedKeys(defs) {
		body, ok := asStringMap(defs[name])
		if !ok {
			return fmt.Errorf("agentdecl: %s: %s.%s must be a mapping of settings", path, FieldRemoteTools, name)
		}
		tool := DeclaredRemoteTool{
			Declared:    name,
			EndpointURL: firstNonEmpty(mapString(body, "url"), mapString(body, "endpointUrl")),
			SpecURL:     firstNonEmpty(mapString(body, "spec"), mapString(body, "specUrl")),
			TimeoutMs:   mapInt(body, "timeoutMs"),
			Headers:     mapStringMap(body, "headers"),
		}
		if tool.EndpointURL == "" && tool.SpecURL == "" {
			return fmt.Errorf("agentdecl: %s: %s.%s names neither url nor spec", path, FieldRemoteTools, name)
		}
		if tool.EndpointURL == "" {
			return fmt.Errorf("agentdecl: %s: %s.%s names a spec but no url to call it on", path, FieldRemoteTools, name)
		}
		ir.DeclaredRemote = append(ir.DeclaredRemote, tool)
	}
	return nil
}

func asStringMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			s, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[s] = val
		}
		return out, true
	}
	return nil, false
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mapString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func mapInt(m map[string]any, key string) int {
	switch t := m[key].(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

func mapList(m map[string]any, key string) []string {
	items, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func mapStringMap(m map[string]any, key string) map[string]string {
	raw, ok := asStringMap(m[key])
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
