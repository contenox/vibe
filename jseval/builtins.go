package jseval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
	"github.com/dop251/goja"
)

// ConsoleBuiltin registers the console object (console.log).
type ConsoleBuiltin struct{}

func (ConsoleBuiltin) Name() string { return "console" }

func (ConsoleBuiltin) Description() string {
	return "Console object with log(...) for debugging. Logs are captured in the sandbox result."
}

func (ConsoleBuiltin) ParametersSchema() map[string]any { return nil }

func (ConsoleBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	return setupConsoleLogger(vm, ctx, tracker, col)
}

// SendEventBuiltin registers sendEvent(eventType, data).
type SendEventBuiltin struct{}

func (SendEventBuiltin) Name() string { return "sendEvent" }

func (SendEventBuiltin) Description() string {
	return "sendEvent(eventType, data): first arg event type (string), second arg payload (object). Appends an event to the event source. Returns { success, event_id } or { success: false, error }."
}

func (SendEventBuiltin) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"eventType": map[string]any{"type": "string", "description": "Event type identifier"},
			"data":      map[string]any{"type": "object", "description": "Event payload as key-value object"},
		},
		"required": []string{"eventType", "data"},
	}
}

func (SendEventBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	if deps.Eventsource == nil {
		return nil
	}
	return vm.Set("sendEvent", func(eventType string, data map[string]any) goja.Value {
		extra := map[string]any{"event_type": eventType, "data": data}
		return withErrorReporting(vm, ctx, tracker, "send", "event", extra, func() (interface{}, error) {
			if col != nil {
				col.Add(ExecLogEntry{
					Timestamp: time.Now().UTC(),
					Kind:      "sendEvent",
					Name:      "sendEvent",
					Args:      []any{eventType, data},
					Meta:      map[string]any{"event_type": eventType},
				})
			}
			_, reportChange, end := tracker.Start(ctx, "send", "event", "event_type", eventType, "data", data)
			defer end()
			dataBytes, err := json.Marshal(data)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "sendEvent", Name: "sendEvent", Error: err.Error(), Meta: map[string]any{"event_type": eventType}})
				}
				return nil, fmt.Errorf("failed to marshal event data: %w", err)
			}
			event := &eventstore.Event{
				ID:            fmt.Sprintf("func-gen-%d", time.Now().UnixNano()),
				CreatedAt:     time.Now().UTC(),
				EventType:     eventType,
				EventSource:   "function_execution",
				AggregateID:   "function",
				AggregateType: "function",
				Version:       1,
				Data:          dataBytes,
				Metadata:      json.RawMessage(`{"source": "function_execution"}`),
			}
			if err := deps.Eventsource.AppendEvent(ctx, event); err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "sendEvent", Name: "sendEvent", Error: err.Error(), Meta: map[string]any{"event_type": eventType}})
				}
				return nil, fmt.Errorf("failed to send event: %w", err)
			}
			reportChange("event_sent", map[string]any{"event_type": eventType, "event_id": event.ID})
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "sendEvent", Name: "sendEvent", Meta: map[string]any{"event_type": eventType, "event_id": event.ID}})
			}
			return map[string]any{"success": true, "event_id": event.ID}, nil
		})
	})
}

// CallTaskChainBuiltin registers callTaskChain(chainID, input).
type CallTaskChainBuiltin struct{}

func (CallTaskChainBuiltin) Name() string { return "callTaskChain" }

func (CallTaskChainBuiltin) Description() string {
	return "Looks up a task chain by ID and returns metadata. Does not execute the chain."
}

func (CallTaskChainBuiltin) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"chainID": map[string]any{"type": "string", "description": "Task chain identifier"},
			"input":   map[string]any{"type": "object", "description": "Input payload for the chain"},
		},
		"required": []string{"chainID", "input"},
	}
}

func (CallTaskChainBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	if deps.TaskchainService == nil {
		return nil
	}
	return vm.Set("callTaskChain", func(chainID string, input map[string]any) goja.Value {
		extra := map[string]any{"chain_id": chainID, "input": input}
		return withErrorReporting(vm, ctx, tracker, "call", "task_chain", extra, func() (interface{}, error) {
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "callTaskChain", Name: "callTaskChain", Args: []any{chainID, input}})
			}
			_, reportChange, end := tracker.Start(ctx, "call", "task_chain", "chain_id", chainID, "input", input)
			defer end()
			chain, err := deps.TaskchainService.Get(ctx, chainID)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "callTaskChain", Name: "callTaskChain", Error: err.Error()})
				}
				return nil, fmt.Errorf("failed to get task chain %s: %w", chainID, err)
			}
			reportChange("task_chain_called", map[string]any{"chain_id": chainID, "chain": chain, "input": input})
			return map[string]any{"success": true, "chain_id": chainID}, nil
		})
	})
}

// ExecuteTaskBuiltin registers executeTask(prompt, modelName, modelProvider).
type ExecuteTaskBuiltin struct{}

func (ExecuteTaskBuiltin) Name() string { return "executeTask" }

func (ExecuteTaskBuiltin) Description() string {
	return "Runs a single LLM task with the given prompt and model. Returns { success, task_id, response }."
}

func (ExecuteTaskBuiltin) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":         map[string]any{"type": "string", "description": "User or system prompt"},
			"modelName":      map[string]any{"type": "string", "description": "Model name (e.g. qwen2.5:7b)"},
			"modelProvider":  map[string]any{"type": "string", "description": "Provider (e.g. ollama)"},
		},
		"required": []string{"prompt", "modelName", "modelProvider"},
	}
}

func (ExecuteTaskBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	if deps.TaskService == nil {
		return nil
	}
	return vm.Set("executeTask", func(prompt, modelName, modelProvider string) goja.Value {
		extra := map[string]any{"prompt": prompt, "model_name": modelName, "model_provider": modelProvider}
		return withErrorReporting(vm, ctx, tracker, "execute", "task", extra, func() (interface{}, error) {
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTask", Name: "executeTask", Args: []any{prompt, modelName, modelProvider}})
			}
			_, reportChange, end := tracker.Start(ctx, "execute", "task", "prompt", prompt, "model_name", modelName, "model_provider", modelProvider)
			defer end()
			req := &execservice.TaskRequest{Prompt: prompt, ModelName: modelName, ModelProvider: modelProvider}
			resp, err := deps.TaskService.Execute(ctx, req)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTask", Name: "executeTask", Error: err.Error()})
				}
				return nil, fmt.Errorf("failed to execute task: %w", err)
			}
			reportChange("task_executed", map[string]any{"task_id": resp.ID, "response": resp.Response})
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTask", Name: "executeTask", Meta: map[string]any{"task_id": resp.ID}})
			}
			return map[string]any{"success": true, "task_id": resp.ID, "response": resp.Response}, nil
		})
	})
}

// ExecuteTaskChainBuiltin registers executeTaskChain(chainID, input).
type ExecuteTaskChainBuiltin struct{}

func (ExecuteTaskChainBuiltin) Name() string { return "executeTaskChain" }

func (ExecuteTaskChainBuiltin) Description() string {
	return "Executes a task chain and returns the final result and history. Returns { success, chain_id, result, history }."
}

func (ExecuteTaskChainBuiltin) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"chainID": map[string]any{"type": "string", "description": "Task chain identifier"},
			"input":   map[string]any{"type": "object", "description": "Input payload"},
		},
		"required": []string{"chainID", "input"},
	}
}

func (ExecuteTaskChainBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	if deps.TaskchainService == nil || deps.TaskchainExecService == nil {
		return nil
	}
	return vm.Set("executeTaskChain", func(chainID string, input map[string]any) goja.Value {
		extra := map[string]any{"chain_id": chainID, "input": input}
		return withErrorReporting(vm, ctx, tracker, "execute", "task_chain", extra, func() (interface{}, error) {
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTaskChain", Name: "executeTaskChain", Args: []any{chainID, input}})
			}
			_, reportChange, end := tracker.Start(ctx, "execute", "task_chain", "chain_id", chainID, "input", input)
			defer end()
			chain, err := deps.TaskchainService.Get(ctx, chainID)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTaskChain", Name: "executeTaskChain", Error: err.Error()})
				}
				return nil, fmt.Errorf("failed to get task chain %s: %w", chainID, err)
			}
			result, resultType, history, err := deps.TaskchainExecService.Execute(ctx, chain, input, taskengine.DataTypeJSON)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTaskChain", Name: "executeTaskChain", Error: err.Error()})
				}
				return nil, fmt.Errorf("failed to execute task chain %s: %w", chainID, err)
			}
			var jsResult interface{}
			switch resultType {
			case taskengine.DataTypeString:
				jsResult = result.(string)
			case taskengine.DataTypeJSON:
				if jsonBytes, ok := result.([]byte); ok {
					var jsonData map[string]any
					if err := json.Unmarshal(jsonBytes, &jsonData); err == nil {
						jsResult = jsonData
					} else {
						jsResult = string(jsonBytes)
					}
				} else if str, ok := result.(string); ok {
					var jsonData map[string]any
					if err := json.Unmarshal([]byte(str), &jsonData); err == nil {
						jsResult = jsonData
					} else {
						jsResult = str
					}
				} else {
					jsResult = result
				}
			default:
				jsResult = result
			}
			reportChange("task_chain_executed", map[string]any{"chain_id": chainID, "result": jsResult, "history": history})
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeTaskChain", Name: "executeTaskChain", Meta: map[string]any{"chain_id": chainID}})
			}
			return map[string]any{"success": true, "chain_id": chainID, "result": jsResult, "history": history}, nil
		})
	})
}

// ExecuteHookBuiltin registers executeHook(hookName, toolName, args).
type ExecuteHookBuiltin struct{}

func (ExecuteHookBuiltin) Name() string { return "executeHook" }

func (ExecuteHookBuiltin) Description() string {
	return "Calls a registered hook's tool by name. Returns { success, hook_name, tool_name, type, result }. Use GetToolsForHookByName for available (hookName, toolName) and their parameters."
}

func (ExecuteHookBuiltin) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"hookName": map[string]any{"type": "string", "description": "Hook name (e.g. local_shell, js_execution)"},
			"toolName": map[string]any{"type": "string", "description": "Tool name exposed by that hook"},
			"args":     map[string]any{"type": "object", "description": "Arguments for the tool"},
		},
		"required": []string{"hookName", "toolName", "args"},
	}
}

func (ExecuteHookBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	if deps.HookRepo == nil {
		return nil
	}
	return vm.Set("executeHook", func(hookName, toolName string, args map[string]any) goja.Value {
		extra := map[string]any{"hook_name": hookName, "tool_name": toolName, "args": args}
		return withErrorReporting(vm, ctx, tracker, "execute", "hook", extra, func() (interface{}, error) {
			if col != nil {
				col.Add(ExecLogEntry{
					Timestamp: time.Now().UTC(),
					Kind:      "executeHook",
					Name:      "executeHook",
					Args:      []any{hookName, toolName, args},
					Meta:      map[string]any{"hook_name": hookName, "tool_name": toolName},
				})
			}
			if supportedHooks, err := deps.HookRepo.Supports(ctx); err == nil && len(supportedHooks) > 0 {
				found := false
				for _, h := range supportedHooks {
					if h == hookName {
						found = true
						break
					}
				}
				if !found {
					msg := fmt.Sprintf("INVALID_HOOK_NAME: %q is not registered; available hooks: %s", hookName, strings.Join(supportedHooks, ", "))
					if col != nil {
						col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeHook", Name: "executeHook", Error: msg, Meta: map[string]any{"hook_name": hookName, "tool_name": toolName}})
					}
					return nil, fmt.Errorf("%s", msg)
				}
			}
			if tools, err := deps.HookRepo.GetToolsForHookByName(ctx, hookName); err == nil && len(tools) > 0 {
				validTool := false
				availableToolNames := make([]string, 0, len(tools))
				for _, t := range tools {
					availableToolNames = append(availableToolNames, t.Function.Name)
					if t.Function.Name == toolName {
						validTool = true
					}
				}
				if !validTool {
					msg := fmt.Sprintf("INVALID_HOOK_TOOL: %q is not a valid tool for hook %q; available tools: %s", toolName, hookName, strings.Join(availableToolNames, ", "))
					if col != nil {
						col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeHook", Name: "executeHook", Error: msg, Meta: map[string]any{"hook_name": hookName, "tool_name": toolName}})
					}
					return nil, fmt.Errorf("%s", msg)
				}
			}
			_, reportChange, end := tracker.Start(ctx, "execute", "hook", "hook_name", hookName, "tool_name", toolName, "args", args)
			defer end()
			argsStr := map[string]string{}
			for k, v := range args {
				argsStr[k] = fmt.Sprintf("%v", v)
			}
			call := &taskengine.HookCall{Name: hookName, ToolName: toolName, Args: argsStr}
			result, dataType, err := deps.HookRepo.Exec(ctx, time.Now().UTC(), nil, false, call)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeHook", Name: "executeHook", Error: err.Error(), Meta: map[string]any{"hook_name": hookName, "tool_name": toolName}})
				}
				return nil, fmt.Errorf("failed to execute hook %s/%s: %w", hookName, toolName, err)
			}
			var jsResult any = result
			switch dataType {
			case taskengine.DataTypeJSON:
				switch r := result.(type) {
				case []byte:
					var v any
					if err := json.Unmarshal(r, &v); err == nil {
						jsResult = v
					}
				case string:
					var v any
					if err := json.Unmarshal([]byte(r), &v); err == nil {
						jsResult = v
					}
				}
			}
			reportChange("hook_executed", map[string]any{"hook_name": hookName, "tool_name": toolName, "type": dataType, "result": jsResult})
			if col != nil {
				col.Add(ExecLogEntry{Timestamp: time.Now().UTC(), Kind: "executeHook", Name: "executeHook", Meta: map[string]any{"hook_name": hookName, "tool_name": toolName}})
			}
			return map[string]any{"success": true, "hook_name": hookName, "tool_name": toolName, "type": dataType, "result": jsResult}, nil
		})
	})
}

// HTTPFetchBuiltin registers httpFetch(urlOrOptions).
type HTTPFetchBuiltin struct{}

func (HTTPFetchBuiltin) Name() string { return "httpFetch" }

func (HTTPFetchBuiltin) Description() string {
	return "Makes an HTTP request. Call with a URL string or an object { url, method?, headers?, body?, timeoutMs? }. Returns { ok, status, statusText, url, headers, body, error? }."
}

func (HTTPFetchBuiltin) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"description": "Either a URL string or { url, method?, headers?, body?, timeoutMs? }",
		"properties": map[string]any{
			"url":       map[string]any{"type": "string", "description": "Request URL"},
			"method":    map[string]any{"type": "string", "description": "HTTP method (default GET)"},
			"headers":   map[string]any{"type": "object", "description": "Request headers"},
			"body":      map[string]any{"type": "string", "description": "Request body"},
			"timeoutMs": map[string]any{"type": "integer", "description": "Timeout in milliseconds"},
		},
	}
}

func (HTTPFetchBuiltin) Register(vm *goja.Runtime, ctx context.Context, tracker libtracker.ActivityTracker, col *Collector, deps BuiltinHandlers) error {
	return setupHTTPFetch(vm, ctx, tracker, col, nil)
}

// DefaultBuiltins returns the default set of builtins in registration order.
func DefaultBuiltins() []Builtin {
	return []Builtin{
		ConsoleBuiltin{},
		SendEventBuiltin{},
		CallTaskChainBuiltin{},
		ExecuteTaskBuiltin{},
		ExecuteTaskChainBuiltin{},
		ExecuteHookBuiltin{},
		HTTPFetchBuiltin{},
	}
}
