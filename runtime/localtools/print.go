package localtools

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// Print implements a simple tools that returns predefined messages
type Print struct {
	tracker libtracker.ActivityTracker
}

// NewPrint creates a new Print instance
func NewPrint(tracker libtracker.ActivityTracker) taskengine.ToolsRepo {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &Print{tracker: tracker}
}

func (h *Print) Exec(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	_, _, end := h.tracker.Start(ctx, "exec", "print_tools")
	defer end()

	message, ok := toolsCall.Args["message"]
	if !ok {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missing 'message' argument in print tools")
	}

	switch v := input.(type) {
	case string:
		// For string input, return the message as string
		return message, taskengine.DataTypeString, nil
	case taskengine.ChatHistory:
		// For chat history, append the message as a system message
		v.Messages = append(v.Messages, taskengine.Message{
			Role:      "system",
			Content:   message,
			Timestamp: time.Now().UTC(),
		})
		return v, taskengine.DataTypeChatHistory, nil
	default:
		// For any other type, return the message as string
		return message, taskengine.DataTypeString, nil
	}
}

func (h *Print) Supports(ctx context.Context) ([]string, error) {
	return []string{"print"}, nil
}

// GetSchemasForSupportedTools returns OpenAPI schemas for supported tools.
func (h *Print) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	// Print tools doesn't have a schema
	return map[string]*openapi3.T{}, nil
}

// GetToolsForToolsByName returns tools exposed by this tools.
func (h *Print) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != "print" {
		return nil, fmt.Errorf("unknown tools: %s", name)
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "print",
				Description: "Prints a message to the output or adds it as a system message in chat history",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"message": map[string]interface{}{
							"type":        "string",
							"description": "The message to print",
						},
					},
					"required": []string{"message"},
				},
			},
		},
	}, nil
}

var _ taskengine.ToolsRepo = (*Print)(nil)
