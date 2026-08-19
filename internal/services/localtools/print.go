package localtools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

// PrintToolsName is the registered toolset name the allowlist addresses; the
// native- scope is a namespace, so a declared MCP source cannot mint the same
// key.
const PrintToolsName = "native-print"

// ToolPrint is the only tool the toolset declares, and the key a
// [tools_policies] block or a HITL rule addresses it by.
const ToolPrint = "print"

// Print emits a message the caller supplies; it holds no state and reaches
// nothing outside the process.
type Print struct {
	tracker libtracker.ActivityTracker
}

func NewPrint(tracker libtracker.ActivityTracker) taskengine.ToolsRepo {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &Print{tracker: tracker}
}

// Exec stamps every returned error with a fatal-vs-recoverable severity marker,
// as the other toolsets in this package do; every failure here is an argument
// the caller can correct, so none of them is fatal.
func (h *Print) Exec(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := h.execDispatch(ctx, input, toolsCall)
	return res, dt, markSeverity(err)
}

func (h *Print) execDispatch(ctx context.Context, input any, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if toolsCall == nil {
		return nil, taskengine.DataTypeAny, errors.New("print: tools required")
	}

	_, _, end := h.tracker.Start(ctx, "exec", "print_tools")
	defer end()

	// A declarative `tools` task may leave tool_name empty, since the toolset
	// declares exactly one tool; anything else named is a different tool.
	if toolName := toolsCall.ToolName; toolName != "" && toolName != ToolPrint {
		return nil, taskengine.DataTypeAny, fmt.Errorf("print: unknown tool %q; this toolset provides %s", toolName, ToolPrint)
	}

	var message string
	if dynArgs, ok := input.(map[string]any); ok {
		if err := rejectUnknownArgs(ToolPrint, dynArgs, "message"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		if v, ok := dynArgs["message"]; ok {
			switch x := v.(type) {
			case string:
				message = x
			default:
				message = fmt.Sprintf("%v", x)
			}
		}
	}
	if message == "" && toolsCall.Args != nil {
		message = toolsCall.Args["message"]
	}
	if message == "" {
		return nil, taskengine.DataTypeAny, errors.New("missing 'message' argument in print tools")
	}

	if hist, ok := input.(taskengine.ChatHistory); ok {
		hist.Messages = append(hist.Messages, taskengine.Message{
			Role:      "system",
			Content:   message,
			Timestamp: time.Now().UTC(),
		})
		return hist, taskengine.DataTypeChatHistory, nil
	}
	return message, taskengine.DataTypeString, nil
}

// Supports advertises the scoped toolset name alone: a bare "print" would be its
// own allowlist entry, separately addressable and surviving "!native-print".
func (h *Print) Supports(ctx context.Context) ([]string, error) {
	return []string{PrintToolsName}, nil
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract, converted from the descriptor GetToolsForToolsByName hands the model so the two cannot drift.
func (h *Print) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	declared, err := h.GetToolsForToolsByName(ctx, PrintToolsName)
	if err != nil {
		return nil, err
	}
	doc, err := buildToolsetDoc(PrintToolsName, "Print Tools",
		"Emits a message the caller supplies. A test and wiring fixture — it reads nothing, writes nothing, and spawns nothing.",
		declared, []toolSchemaSpec{{tool: ToolPrint, component: "Print", response: printResponseSchema}})
	if err != nil {
		return nil, err
	}
	return map[string]*openapi3.T{PrintToolsName: doc}, nil
}

func printResponseSchema() *openapi3.SchemaRef {
	return oneOfSchema("What print returns, following the shape of its input.",
		strSchema("The message, as given — a non-string argument is rendered with %v."),
		chatHistorySchema("The conversation with the message appended as a system message."))
}

func (h *Print) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != PrintToolsName {
		return nil, fmt.Errorf("unknown tools: %s", name)
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        ToolPrint,
				Description: "Prints a message to the output or adds it as a system message in chat history. Runs on the AGENT HOST and reaches nothing outside the process: no file, no network, no command.",
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
