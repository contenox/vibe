package enginebridge

import (
	"context"
	"errors"

	libacp "github.com/contenox/contenox/libacp"
)

// errNoWorkspace answers an fs or terminal call arriving without a workspace
// root. Initialize advertised neither capability then, so reaching here means
// the agent ignored the advertisement.
var errNoWorkspace = errors.New("enginebridge: no workspace root; fs and terminal were not advertised")

func (c *bridgeClient) ReadTextFile(ctx context.Context, req libacp.ReadTextFileRequest) (libacp.ReadTextFileResponse, error) {
	if c.b.fsterm == nil {
		return libacp.ReadTextFileResponse{}, errNoWorkspace
	}
	return c.b.fsterm.ReadTextFile(ctx, req)
}

func (c *bridgeClient) WriteTextFile(ctx context.Context, req libacp.WriteTextFileRequest) (libacp.WriteTextFileResponse, error) {
	if c.b.fsterm == nil {
		return libacp.WriteTextFileResponse{}, errNoWorkspace
	}
	return c.b.fsterm.WriteTextFile(ctx, req)
}
