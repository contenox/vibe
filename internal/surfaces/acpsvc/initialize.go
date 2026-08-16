package acpsvc

import (
	"context"
	"encoding/json"
	"os"

	"github.com/contenox/contenox/internal/version"
	"github.com/contenox/contenox/libacp"
)

// Initialize negotiates the protocol version and reports the auth methods,
// capabilities and workspace config options this agent offers.
func (t *Transport) Initialize(ctx context.Context, req libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	t.initMu.Lock()
	t.clientInfo = req.ClientInfo
	t.clientCaps = req.ClientCapabilities
	t.initMu.Unlock()

	var authMethods []libacp.AuthMethod
	if clientSupportsTerminalAuth(req.ClientCapabilities) {
		command := os.Args[0]
		authMethods = append(authMethods, libacp.AuthMethod{
			ID:          terminalAuthMethodID,
			Name:        "Setup Contenox",
			Description: "Opens an interactive terminal to configure your LLM provider and model.",
			Type:        libacp.AuthMethodTypeTerminal,
			Args:        []string{"acp", "--setup"},
			Meta: mustJSON(map[string]any{
				"terminal-auth": map[string]any{
					"command": command,
					"args":    []string{"acp", "--setup"},
					"label":   "Contenox Setup",
				},
			}),
		})
	}
	if t.deps.Engine == nil && t.deps.EnvSetup != nil {
		authMethods = append(authMethods, libacp.AuthMethod{
			ID:          envAuthMethodID,
			Name:        "Configure from environment",
			Description: "Set the CONTENOX_DEFAULT_* variables (plus a provider API key for cloud providers); contenox completes setup non-interactively.",
			Type:        libacp.AuthMethodTypeEnvVar,
			Vars:        t.deps.EnvSetup.Vars,
		})
	}

	resp := libacp.InitializeResponse{
		ProtocolVersion: negotiateProtocolVersion(req.ProtocolVersion),
		AgentInfo: &libacp.Implementation{
			Name:    "contenox",
			Title:   "Contenox ACP Agent",
			Version: version.Get(),
		},
		AgentCapabilities: libacp.AgentCapabilities{
			LoadSession: true,
			PromptCapabilities: libacp.PromptCapabilities{
				Image:           true,
				Audio:           true,
				EmbeddedContext: true,
			},
			McpCapabilities: libacp.McpCapabilities{
				HTTP: true,
				SSE:  false,
			},
			SessionCapabilities: libacp.SessionCapabilities{
				List:   &struct{}{},
				Resume: &struct{}{},
				Close:  &struct{}{},
				Delete: &struct{}{},
			},
		},
		AuthMethods: authMethods,
	}

	// Workspace config options let a client render controls before any session
	// exists. Only sent when configured.
	if t.deps.Engine != nil {
		if opts := t.workspaceConfigOptions(ctx); len(opts) > 0 {
			resp.Meta = mustJSON(map[string]any{
				WorkspaceConfigOptionsMetaKey: opts,
			})
		}
	}

	return resp, nil
}

func negotiateProtocolVersion(client int) int {
	if client >= 1 && client <= libacp.ProtocolVersion {
		return client
	}
	return libacp.ProtocolVersion
}

func clientSupportsTerminalAuth(caps libacp.ClientCapabilities) bool {
	if caps.Auth.Terminal {
		return true
	}
	if caps.Meta == nil {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(caps.Meta, &meta); err != nil {
		return false
	}
	v, ok := meta["terminal-auth"]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
