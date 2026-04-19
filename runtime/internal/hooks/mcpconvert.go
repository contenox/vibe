package hooks

import (
	"encoding/json"

	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
)

// mcpToolToTaskTool converts a runtimetypes.MCPTool (received from mcpworker via NATS)
// to a taskengine.Tool.
//
// injectParams keys are stripped from the tool's inputSchema (properties + required list)
// before the schema is shown to the model, so the model never sees injected parameters.
// InputSchema is otherwise passed as-is; the LLM provider handles any schema sanitisation
// it needs (e.g. Gemini strips additionalProperties).
func mcpToolToTaskTool(hookName string, t runtimetypes.MCPTool, injectParams map[string]string) taskengine.Tool {
	_ = hookName // available for future namespacing
	var params any
	if len(t.InputSchema) > 0 {
		params = filterMCPSchema(t.InputSchema, injectParams)
	}
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	}
}

// filterMCPSchema removes keys in injectParams from the inputSchema's "properties"
// and "required" fields. If injectParams is empty, the raw schema is returned as-is.
func filterMCPSchema(rawSchema json.RawMessage, injectParams map[string]string) any {
	if len(injectParams) == 0 {
		// Fast path: nothing to strip.
		var out any
		if err := json.Unmarshal(rawSchema, &out); err == nil {
			return out
		}
		return rawSchema
	}

	var schema map[string]any
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		// Can't parse — return raw, let providers deal with it.
		var out any
		_ = json.Unmarshal(rawSchema, &out)
		return out
	}

	// Strip injected keys from "properties".
	if props, ok := schema["properties"].(map[string]any); ok {
		for k := range injectParams {
			delete(props, k)
		}
		schema["properties"] = props
	}

	// Strip injected keys from "required".
	if reqRaw, ok := schema["required"].([]any); ok {
		filtered := reqRaw[:0]
		for _, v := range reqRaw {
			if s, ok := v.(string); ok {
				if _, injected := injectParams[s]; !injected {
					filtered = append(filtered, v)
				}
			}
		}
		if len(filtered) > 0 {
			schema["required"] = filtered
		} else {
			delete(schema, "required")
		}
	}

	return schema
}
