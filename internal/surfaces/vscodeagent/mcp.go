package vscodeagent

import (
	"context"
	"strings"

	"github.com/contenox/contenox/internal/store/runtimetypes"
)

type mcpServerInfo struct {
	ID                    string            `json:"id,omitempty"`
	Name                  string            `json:"name"`
	Transport             string            `json:"transport"`
	Command               string            `json:"command,omitempty"`
	Args                  []string          `json:"args,omitempty"`
	URL                   string            `json:"url,omitempty"`
	AuthType              string            `json:"authType,omitempty"`
	AuthEnvKey            string            `json:"authEnvKey,omitempty"`
	Headers               map[string]string `json:"headers,omitempty"`
	ConnectTimeoutSeconds int               `json:"connectTimeoutSeconds,omitempty"`
}

type listMCPServersResult struct {
	Servers []mcpServerInfo `json:"servers"`
}

func (s *Server) listMCPServers(ctx context.Context) (listMCPServersResult, error) {
	servers, err := s.store.ListMCPServers(ctx, nil, 1000)
	if err != nil {
		return listMCPServersResult{}, err
	}
	out := make([]mcpServerInfo, 0, len(servers))
	for _, server := range servers {
		if info, ok := mcpServerInfoFromRuntime(server); ok {
			out = append(out, info)
		}
	}
	return listMCPServersResult{Servers: out}, nil
}

func mcpServerInfoFromRuntime(server *runtimetypes.MCPServer) (mcpServerInfo, bool) {
	if server == nil || strings.TrimSpace(server.Name) == "" {
		return mcpServerInfo{}, false
	}
	transport := strings.ToLower(strings.TrimSpace(server.Transport))
	switch transport {
	case "stdio", "http", "sse":
	default:
		return mcpServerInfo{}, false
	}
	headers := map[string]string{}
	for key, value := range server.Headers {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		headers[key] = value
	}
	return mcpServerInfo{
		ID:                    server.ID,
		Name:                  server.Name,
		Transport:             transport,
		Command:               server.Command,
		Args:                  append([]string(nil), server.Args...),
		URL:                   server.URL,
		AuthType:              server.AuthType,
		AuthEnvKey:            server.AuthEnvKey,
		Headers:               headers,
		ConnectTimeoutSeconds: server.ConnectTimeoutSeconds,
	}, true
}
