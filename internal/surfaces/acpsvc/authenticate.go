package acpsvc

import (
	"context"

	libacp "github.com/contenox/beam/libacp"
)

const (
	terminalAuthMethodID = "terminal"
	browserAuthMethodID  = "browser"
	envAuthMethodID      = "env"
)

func (t *Transport) Authenticate(ctx context.Context, req libacp.AuthenticateRequest) (libacp.AuthenticateResponse, error) {
	switch req.MethodID {
	case terminalAuthMethodID, browserAuthMethodID:
		// Both run through the client's terminal-auth mechanics; the browser
		// variant's command hosts the Beam wizard instead of the TUI.
		if clientSupportsTerminalAuth(t.getClientCaps()) {
			return libacp.AuthenticateResponse{}, nil
		}
	case envAuthMethodID:
		// Only honored while it is advertised (setup-only mode with an env
		// setup path wired in).
		if t.deps.Engine == nil && t.deps.EnvSetup != nil && t.deps.EnvSetup.Complete != nil {
			if err := t.deps.EnvSetup.Complete(ctx); err != nil {
				return libacp.AuthenticateResponse{}, libacp.NewErrorf(libacp.ErrAuthRequired, "environment-based setup incomplete: %v", err)
			}
			// Configuration is persisted; this process still runs setup-only,
			// so the client must reconnect for a working engine (same
			// semantics as completing the terminal wizard).
			return libacp.AuthenticateResponse{}, nil
		}
	}
	return libacp.AuthenticateResponse{}, libacp.NewErrorf(libacp.ErrInvalidParams, "auth method %q is not supported; use the terminal method to run --setup", req.MethodID)
}

// Logout is not supported: contenox's auth model (the terminal/browser/env
// setup wizards above) has no persisted "logged in" session to tear down, and
// Initialize never advertises AgentCapabilities.Auth.Logout, so a conformant
// client will never call this.
func (t *Transport) Logout(_ context.Context, _ libacp.LogoutRequest) (libacp.LogoutResponse, error) {
	return libacp.LogoutResponse{}, libacp.MethodNotFound(libacp.MethodLogout)
}
