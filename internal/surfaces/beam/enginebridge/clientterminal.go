package enginebridge

import (
	"context"

	libacp "github.com/contenox/contenox/libacp"
)

// The terminal capability is served by the shared clientfsterm.Server: these
// methods route the agent's terminal/* callbacks to it, or answer errNoWorkspace
// when no workspace root was advertised.

func (c *bridgeClient) CreateTerminal(ctx context.Context, req libacp.CreateTerminalRequest) (libacp.CreateTerminalResponse, error) {
	if c.b.fsterm == nil {
		return libacp.CreateTerminalResponse{}, errNoWorkspace
	}
	return c.b.fsterm.CreateTerminal(ctx, req)
}

func (c *bridgeClient) TerminalOutput(ctx context.Context, req libacp.TerminalOutputRequest) (libacp.TerminalOutputResponse, error) {
	if c.b.fsterm == nil {
		return libacp.TerminalOutputResponse{}, errNoWorkspace
	}
	return c.b.fsterm.TerminalOutput(ctx, req)
}

func (c *bridgeClient) WaitForTerminalExit(ctx context.Context, req libacp.WaitForTerminalExitRequest) (libacp.WaitForTerminalExitResponse, error) {
	if c.b.fsterm == nil {
		return libacp.WaitForTerminalExitResponse{}, errNoWorkspace
	}
	return c.b.fsterm.WaitForTerminalExit(ctx, req)
}

func (c *bridgeClient) KillTerminal(ctx context.Context, req libacp.KillTerminalRequest) (libacp.KillTerminalResponse, error) {
	if c.b.fsterm == nil {
		return libacp.KillTerminalResponse{}, errNoWorkspace
	}
	return c.b.fsterm.KillTerminal(ctx, req)
}

func (c *bridgeClient) ReleaseTerminal(ctx context.Context, req libacp.ReleaseTerminalRequest) (libacp.ReleaseTerminalResponse, error) {
	if c.b.fsterm == nil {
		return libacp.ReleaseTerminalResponse{}, errNoWorkspace
	}
	return c.b.fsterm.ReleaseTerminal(ctx, req)
}
