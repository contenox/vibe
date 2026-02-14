package hooks

import (
	"context"
	"net/http"

	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// ToolProtocol defines the methods required to interact with a remote service
// that exposes tools via a standardized protocol (e.g., OpenAPI).
// It is responsible for discovering available tools and executing tool calls.
// ToolProtocol defines the interface for interacting with remote tools via OpenAPI.
type ToolProtocol interface {
	FetchSchema(ctx context.Context, endpointURL string, httpClient *http.Client) (*openapi3.T, error)
	FetchTools(ctx context.Context, endpointURL string, injectParams map[string]ParamArg, httpClient *http.Client) ([]taskengine.Tool, error)
	ExecuteTool(
		ctx context.Context,
		endpointURL string,
		httpClient *http.Client,
		injectParams map[string]ParamArg,
		toolCall taskengine.ToolCall,
	) (interface{}, taskengine.DataType, error)
}
