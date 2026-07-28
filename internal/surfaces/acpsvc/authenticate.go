package acpsvc

import (
	"context"

	libacp "github.com/contenox/beam/libacp"
)

const (
	terminalAuthMethodID = "terminal"
	envAuthMethodID      = "env"
)

// Authenticate accepts only the method IDs Initialize advertised; anything
// else, including a retired or unknown method, falls through to the
// unsupported-method error below.
func (t *Transport) Authenticate(ctx context.Context, req libacp.AuthenticateRequest) (libacp.AuthenticateResponse, error) {
	switch req.MethodID {
	case terminalAuthMethodID:
		if clientSupportsTerminalAuth(t.getClientCaps()) {
			return libacp.AuthenticateResponse{}, nil
		}
	case envAuthMethodID:
		// Honored only in setup-only mode with an env path wired in.
		if t.deps.Engine == nil && t.deps.EnvSetup != nil && t.deps.EnvSetup.Complete != nil {
			if err := t.deps.EnvSetup.Complete(ctx); err != nil {
				return libacp.AuthenticateResponse{}, libacp.NewErrorf(libacp.ErrAuthRequired, "environment-based setup incomplete: %v", err)
			}
			// Config is persisted, but this process still runs setup-only;
			// the client must reconnect for a working engine.
			return libacp.AuthenticateResponse{}, nil
		}
	}
	return libacp.AuthenticateResponse{}, libacp.NewErrorf(libacp.ErrInvalidParams, "auth method %q is not supported; use the terminal method to run --setup", req.MethodID)
}

// Logout is unsupported: contenox's auth model has no persisted session to
// tear down, and Initialize never advertises AgentCapabilities.Auth.Logout.
func (t *Transport) Logout(_ context.Context, _ libacp.LogoutRequest) (libacp.LogoutResponse, error) {
	return libacp.LogoutResponse{}, libacp.MethodNotFound(libacp.MethodLogout)
}
