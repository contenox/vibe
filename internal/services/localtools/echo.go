package localtools

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// EchoTools is a simple tools that echoes back the input arguments.
type EchoTools struct{}

// NewEchoTools creates a new instance of EchoTools.
func NewEchoTools() taskengine.ToolsRepo {
	return &EchoTools{}
}

// Exec handles execution by echoing the input arguments.
func (e *EchoTools) Exec(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	switch v := input.(type) {
	case map[string]any:
		if err := rejectUnknownArgs("echo", v, "input"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		if raw, ok := v["input"]; ok {
			switch x := raw.(type) {
			case string:
				return x, taskengine.DataTypeString, nil
			default:
				return fmt.Sprintf("%v", x), taskengine.DataTypeString, nil
			}
		}
		return "nothing to echo", taskengine.DataTypeString, nil
	case string:
		return v, taskengine.DataTypeString, nil
	case taskengine.ChatHistory:
		var echoContent string
		for i := len(v.Messages) - 1; i >= 0; i-- {
			if v.Messages[i].Role == "user" {
				echoContent = v.Messages[i].Content
				break
			}
		}
		if echoContent == "" {
			echoContent = "nothing to echo"
		}
		v.Messages = append(v.Messages, taskengine.Message{
			Role:      "assistant",
			Content:   "Echo: " + echoContent,
			Timestamp: time.Now().UTC(),
		})
		return v, taskengine.DataTypeChatHistory, nil
	default:
		return fmt.Sprintf("%v", input), taskengine.DataTypeString, nil
	}
}

// Supports returns the tools types supported by this tools.
func (e *EchoTools) Supports(ctx context.Context) ([]string, error) {
	return []string{"echo"}, nil
}

// GetSchemasForSupportedTools returns OpenAPI schemas for supported tools.
func (e *EchoTools) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	// Echo tools doesn't have a schema
	return map[string]*openapi3.T{}, nil
}

// GetToolsForToolsByName returns tools exposed by this tools.
func (e *EchoTools) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != "echo" {
		return nil, fmt.Errorf("unknown tools: %s", name)
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "echo",
				Description: "Echoes back the input message or the last user message from chat history",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"input": map[string]interface{}{
							"type":        "string",
							"description": "The input to echo back",
						},
					},
					"required": []string{"input"},
				},
			},
		},
	}, nil
}

var _ taskengine.ToolsRepo = (*EchoTools)(nil)
