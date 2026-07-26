package agenthost

import (
	"context"
	"fmt"
	"sort"

	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
)

// McpServerResolver is the narrow lookup surface ResolveForwardedMcpServers
// needs; mcpserverservice.Service satisfies it. Declared here so this package
// stays free of a service dependency.
type McpServerResolver interface {
	GetByName(ctx context.Context, name string) (*runtimetypes.MCPServer, error)
}

// ResolveForwardedMcpServers turns an agent's mcp_servers allowlist
// (runtimetypes.ExternalACPConfig.McpServers) into the libacp.McpServer
// entries to pass down in ACP session/new. A name that resolves to nothing is
// a loud error, not a silent skip: the allowlist is consent the user wrote
// down, and dropping an entry unnoticed would mean checking/driving the agent
// with less context than they declared.
func ResolveForwardedMcpServers(ctx context.Context, resolver McpServerResolver, names []string) ([]libacp.McpServer, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]libacp.McpServer, 0, len(names))
	for _, name := range names {
		row, err := resolver.GetByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("agenthost: mcp server %q from the agent's mcp_servers allowlist: %w", name, err)
		}
		srv, err := McpServerForACP(row)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, nil
}

// McpServerForACP maps a registered MCP server row to its ACP session/new
// wire shape — the inverse of acpsvc's mcpRowFromLibacp (the direction where
// contenox is the agent receiving servers from an editor).
//
// Only reachability data is forwarded: argv for stdio, URL plus explicitly
// configured headers for http/sse. Contenox-side auth synthesis (authToken,
// authEnvKey, oauth client material) and injectParams are machinery of
// contenox's own MCP client connections and are deliberately NOT translated
// into the payload — if a hosted agent needs credentials for a server, they
// must be written into the row's headers explicitly, where forwarding them is
// a visible choice.
func McpServerForACP(row *runtimetypes.MCPServer) (libacp.McpServer, error) {
	srv := libacp.McpServer{Name: row.Name}
	switch row.Transport {
	case "stdio":
		if row.Command == "" {
			return libacp.McpServer{}, fmt.Errorf("agenthost: mcp server %q: stdio transport without a command", row.Name)
		}
		srv.Command = row.Command
		srv.Args = append([]string{}, row.Args...)
		srv.Env = []libacp.EnvVariable{}
	case "http", "sse":
		if row.URL == "" {
			return libacp.McpServer{}, fmt.Errorf("agenthost: mcp server %q: %s transport without a url", row.Name, row.Transport)
		}
		srv.Type = row.Transport
		srv.URL = row.URL
		srv.Headers = make([]libacp.HttpHeader, 0, len(row.Headers))
		for name, value := range row.Headers {
			srv.Headers = append(srv.Headers, libacp.HttpHeader{Name: name, Value: value})
		}
		// Deterministic wire order: map iteration would reorder headers run
		// to run, which makes byte-level assertions and diffs needlessly
		// noisy.
		sort.Slice(srv.Headers, func(i, j int) bool { return srv.Headers[i].Name < srv.Headers[j].Name })
	default:
		return libacp.McpServer{}, fmt.Errorf("agenthost: mcp server %q: unsupported transport %q for ACP forwarding", row.Name, row.Transport)
	}
	return srv, nil
}

// filterMcpServersByCapabilities drops forwarded servers the agent cannot
// consume, per its initialize-advertised mcpCapabilities: stdio is the
// protocol baseline and always passes; http and sse require the matching
// capability flag. Returned slices carry the kept servers and the names of
// the dropped ones, so callers can report the filtering instead of the agent
// silently missing servers the user allowlisted.
func filterMcpServersByCapabilities(servers []libacp.McpServer, caps libacp.McpCapabilities) (kept []libacp.McpServer, dropped []string) {
	for _, srv := range servers {
		switch srv.Kind() {
		case libacp.McpServerKindHTTP:
			if !caps.HTTP {
				dropped = append(dropped, srv.Name)
				continue
			}
		case libacp.McpServerKindSSE:
			if !caps.SSE {
				dropped = append(dropped, srv.Name)
				continue
			}
		}
		kept = append(kept, srv)
	}
	return kept, dropped
}
