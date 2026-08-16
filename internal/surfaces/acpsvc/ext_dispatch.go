package acpsvc

import (
	"context"
	"encoding/json"

	libacp "github.com/contenox/contenox/libacp"
)

func (t *Transport) handleExtRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	switch method {
	case extMethodAutocomplete:
		return t.handleAutocomplete(ctx, params)
	default:
		return nil, libacp.MethodNotFound(method)
	}
}
