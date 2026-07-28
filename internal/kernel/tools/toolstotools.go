package tools

import (
	"context"
	"net/http"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// ToolProtocol discovers and executes tool calls against a remote service
// exposing tools via a standardized protocol (e.g. OpenAPI).
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
