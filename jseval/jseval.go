package jseval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
	"github.com/dop251/goja"
)

type ActionFunc func() (interface{}, error)

func withErrorReporting(
	vm *goja.Runtime,
	ctx context.Context,
	tracker libtracker.ActivityTracker,
	operation, subject string,
	extra map[string]any,
	fn ActionFunc,
) goja.Value {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resultChan := make(chan struct {
		result interface{}
		err    error
	}, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				var err error
				switch x := r.(type) {
				case string:
					err = fmt.Errorf("panic: %s", x)
				case error:
					err = x
				default:
					err = fmt.Errorf("panic: %v", x)
				}

				fields := map[string]any{"recovered_panic": true}
				for k, v := range extra {
					fields[k] = v
				}
				reportErr, _, end := tracker.Start(ctx, operation, subject, fields)
				defer end()
				reportErr(fmt.Errorf("recovered panic in %s %s: %w", operation, subject, err))

				resultChan <- struct {
					result interface{}
					err    error
				}{nil, err}
			}
		}()

		result, err := fn()
		resultChan <- struct {
			result interface{}
			err    error
		}{result, err}
	}()

	select {
	case <-timeoutCtx.Done():
		fields := map[string]any{"timeout": true}
		for k, v := range extra {
			fields[k] = v
		}
		reportErr, _, end := tracker.Start(ctx, operation, subject, fields)
		defer end()
		reportErr(fmt.Errorf("operation timed out after 30s in %s %s", operation, subject))

		return vm.ToValue(map[string]any{
			"error":   fmt.Sprintf("%s %s timed out after 30s", operation, subject),
			"success": false,
		})

	case res := <-resultChan:
		if res.err != nil {
			fields := map[string]any{"error_occurred": true}
			for k, v := range extra {
				fields[k] = v
			}
			reportErr, _, end := tracker.Start(ctx, operation, subject, fields)
			defer end()
			reportErr(res.err)

			return vm.ToValue(map[string]any{
				"error":   res.err.Error(),
				"success": false,
			})
		}

		if res.result == nil {
			return goja.Undefined()
		}

		return vm.ToValue(res.result)
	}
}

func setupConsoleLogger(
	vm *goja.Runtime,
	ctx context.Context,
	tracker libtracker.ActivityTracker,
	col *Collector,
) error {
	consoleObj := vm.NewObject()
	if err := consoleObj.Set("log", func(call goja.FunctionCall) goja.Value {
		args := make([]interface{}, len(call.Arguments))
		for i, arg := range call.Arguments {
			args[i] = arg.Export()
		}

		// collector entry
		if col != nil {
			col.Add(ExecLogEntry{
				Timestamp: time.Now().UTC(),
				Kind:      "console",
				Level:     "log",
				Args:      args,
			})
		}

		extra := map[string]any{"args": args}
		return withErrorReporting(vm, ctx, tracker, "log", "console", extra, func() (interface{}, error) {
			_, _, end := tracker.Start(ctx, "log", "console", "args", args)
			defer end()
			return nil, nil
		})
	}); err != nil {
		return fmt.Errorf("failed to set console.log: %w", err)
	}
	if err := vm.Set("console", consoleObj); err != nil {
		return fmt.Errorf("failed to set console object: %w", err)
	}
	return nil
}

// BuiltinHandlers holds the REAL services used by the builtins.
// These are injected once when you create the env.
type BuiltinHandlers struct {
	Eventsource          eventsourceservice.Service
	TaskService          execservice.ExecService
	TaskchainService     taskchainservice.Service
	TaskchainExecService execservice.TasksEnvService
	FunctionService      functionservice.Service // not used yet, but available

	HookRepo taskengine.HookRepo
}

// Env = configured JS environment with tracker + services.
type Env struct {
	tracker libtracker.ActivityTracker
	deps    BuiltinHandlers
}

func NewEnv(
	tracker libtracker.ActivityTracker,
	deps BuiltinHandlers,
) *Env {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &Env{
		tracker: tracker,
		deps:    deps,
	}
}

// SetupVM wires console + builtins into the given VM using the env’s deps.
// Everything they do gets appended to the Collector.
func (e *Env) SetupVM(ctx context.Context, vm *goja.Runtime, col *Collector) error {
	if vm == nil {
		return fmt.Errorf("vm is nil")
	}

	// console.log
	if err := setupConsoleLogger(vm, ctx, e.tracker, col); err != nil {
		reportErr, _, end := e.tracker.Start(ctx, "setup", "console_logger_error", "err", err.Error())
		defer end()
		reportErr(err)
	}

	// sendEvent
	if e.deps.Eventsource != nil {
		if err := vm.Set("sendEvent", func(eventType string, data map[string]any) goja.Value {
			extra := map[string]any{"event_type": eventType, "data": data}
			return withErrorReporting(vm, ctx, e.tracker, "send", "event", extra, func() (interface{}, error) {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "sendEvent",
						Name:      "sendEvent",
						Args:      []any{eventType, data},
						Meta: map[string]any{
							"event_type": eventType,
						},
					})
				}

				_, reportChange, end := e.tracker.Start(ctx, "send", "event",
					"event_type", eventType, "data", data)
				defer end()

				dataBytes, err := json.Marshal(data)
				if err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "sendEvent",
							Name:      "sendEvent",
							Error:     err.Error(),
							Meta: map[string]any{
								"event_type": eventType,
							},
						})
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

				if err := e.deps.Eventsource.AppendEvent(ctx, event); err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "sendEvent",
							Name:      "sendEvent",
							Error:     err.Error(),
							Meta: map[string]any{
								"event_type": eventType,
							},
						})
					}
					return nil, fmt.Errorf("failed to send event: %w", err)
				}

				reportChange("event_sent", map[string]any{
					"event_type": eventType,
					"event_id":   event.ID,
				})

				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "sendEvent",
						Name:      "sendEvent",
						Meta: map[string]any{
							"event_type": eventType,
							"event_id":   event.ID,
						},
					})
				}

				return map[string]any{
					"success":  true,
					"event_id": event.ID,
				}, nil
			})
		}); err != nil {
			return err
		}
	}

	// callTaskChain
	if e.deps.TaskchainService != nil {
		if err := vm.Set("callTaskChain", func(chainID string, input map[string]any) goja.Value {
			extra := map[string]any{"chain_id": chainID, "input": input}
			return withErrorReporting(vm, ctx, e.tracker, "call", "task_chain", extra, func() (interface{}, error) {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "callTaskChain",
						Name:      "callTaskChain",
						Args:      []any{chainID, input},
					})
				}

				_, reportChange, end := e.tracker.Start(ctx, "call", "task_chain",
					"chain_id", chainID, "input", input)
				defer end()

				chain, err := e.deps.TaskchainService.Get(ctx, chainID)
				if err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "callTaskChain",
							Name:      "callTaskChain",
							Error:     err.Error(),
						})
					}
					return nil, fmt.Errorf("failed to get task chain %s: %w", chainID, err)
				}

				reportChange("task_chain_called", map[string]any{
					"chain_id": chainID,
					"chain":    chain,
					"input":    input,
				})

				return map[string]any{
					"success":  true,
					"chain_id": chainID,
				}, nil
			})
		}); err != nil {
			return err
		}
	}

	// executeTask
	if e.deps.TaskService != nil {
		if err := vm.Set("executeTask", func(prompt, modelName, modelProvider string) goja.Value {
			extra := map[string]any{
				"prompt":         prompt,
				"model_name":     modelName,
				"model_provider": modelProvider,
			}
			return withErrorReporting(vm, ctx, e.tracker, "execute", "task", extra, func() (interface{}, error) {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "executeTask",
						Name:      "executeTask",
						Args:      []any{prompt, modelName, modelProvider},
					})
				}

				_, reportChange, end := e.tracker.Start(ctx, "execute", "task",
					"prompt", prompt, "model_name", modelName, "model_provider", modelProvider)
				defer end()

				req := &execservice.TaskRequest{
					Prompt:        prompt,
					ModelName:     modelName,
					ModelProvider: modelProvider,
				}

				resp, err := e.deps.TaskService.Execute(ctx, req)
				if err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "executeTask",
							Name:      "executeTask",
							Error:     err.Error(),
						})
					}
					return nil, fmt.Errorf("failed to execute task: %w", err)
				}

				reportChange("task_executed", map[string]any{
					"task_id":  resp.ID,
					"response": resp.Response,
				})

				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "executeTask",
						Name:      "executeTask",
						Meta: map[string]any{
							"task_id": resp.ID,
						},
					})
				}

				return map[string]any{
					"success":  true,
					"task_id":  resp.ID,
					"response": resp.Response,
				}, nil
			})
		}); err != nil {
			return err
		}
	}

	// executeTaskChain
	if e.deps.TaskchainService != nil && e.deps.TaskchainExecService != nil {
		if err := vm.Set("executeTaskChain", func(chainID string, input map[string]any) goja.Value {
			extra := map[string]any{"chain_id": chainID, "input": input}
			return withErrorReporting(vm, ctx, e.tracker, "execute", "task_chain", extra, func() (interface{}, error) {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "executeTaskChain",
						Name:      "executeTaskChain",
						Args:      []any{chainID, input},
					})
				}

				_, reportChange, end := e.tracker.Start(ctx, "execute", "task_chain",
					"chain_id", chainID, "input", input)
				defer end()

				chain, err := e.deps.TaskchainService.Get(ctx, chainID)
				if err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "executeTaskChain",
							Name:      "executeTaskChain",
							Error:     err.Error(),
						})
					}
					return nil, fmt.Errorf("failed to get task chain %s: %w", chainID, err)
				}

				result, resultType, history, err := e.deps.TaskchainExecService.Execute(
					ctx,
					chain,
					input,
					taskengine.DataTypeJSON,
				)
				if err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "executeTaskChain",
							Name:      "executeTaskChain",
							Error:     err.Error(),
						})
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

				reportChange("task_chain_executed", map[string]any{
					"chain_id": chainID,
					"result":   jsResult,
					"history":  history,
				})

				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "executeTaskChain",
						Name:      "executeTaskChain",
						Meta: map[string]any{
							"chain_id": chainID,
						},
					})
				}

				return map[string]any{
					"success":  true,
					"chain_id": chainID,
					"result":   jsResult,
					"history":  history,
				}, nil
			})
		}); err != nil {
			return err
		}
	}

	// executeHook
	if e.deps.HookRepo != nil {
		if err := vm.Set("executeHook", func(hookName, toolName string, args map[string]any) goja.Value {
			extra := map[string]any{
				"hook_name": hookName,
				"tool_name": toolName,
				"args":      args,
			}

			return withErrorReporting(vm, ctx, e.tracker, "execute", "hook", extra, func() (interface{}, error) {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "executeHook",
						Name:      "executeHook",
						Args:      []any{hookName, toolName, args},
						Meta: map[string]any{
							"hook_name": hookName,
							"tool_name": toolName,
						},
					})
				}

				// 1) Validate hookName against supported hooks (if we can).
				if e.deps.HookRepo != nil {
					if supportedHooks, err := e.deps.HookRepo.Supports(ctx); err == nil && len(supportedHooks) > 0 {
						found := false
						for _, h := range supportedHooks {
							if h == hookName {
								found = true
								break
							}
						}
						if !found {
							msg := fmt.Sprintf(
								"INVALID_HOOK_NAME: %q is not registered; available hooks: %s",
								hookName,
								strings.Join(supportedHooks, ", "),
							)

							if col != nil {
								col.Add(ExecLogEntry{
									Timestamp: time.Now().UTC(),
									Kind:      "executeHook",
									Name:      "executeHook",
									Error:     msg,
									Meta: map[string]any{
										"hook_name": hookName,
										"tool_name": toolName,
									},
								})
							}

							return nil, fmt.Errorf("%s", msg)
						}
					}
				}

				// 2) Validate toolName against tools for this hook (if we can).
				if e.deps.HookRepo != nil {
					if tools, err := e.deps.HookRepo.GetToolsForHookByName(ctx, hookName); err == nil && len(tools) > 0 {
						validTool := false
						availableToolNames := make([]string, 0, len(tools))
						for _, t := range tools {
							availableToolNames = append(availableToolNames, t.Function.Name)
							if t.Function.Name == toolName {
								validTool = true
							}
						}
						if !validTool {
							msg := fmt.Sprintf(
								"INVALID_HOOK_TOOL: %q is not a valid tool for hook %q; available tools: %s",
								toolName,
								hookName,
								strings.Join(availableToolNames, ", "),
							)

							if col != nil {
								col.Add(ExecLogEntry{
									Timestamp: time.Now().UTC(),
									Kind:      "executeHook",
									Name:      "executeHook",
									Error:     msg,
									Meta: map[string]any{
										"hook_name": hookName,
										"tool_name": toolName,
									},
								})
							}

							return nil, fmt.Errorf("%s", msg)
						}
					}
				}

				_, reportChange, end := e.tracker.Start(
					ctx,
					"execute",
					"hook",
					"hook_name", hookName,
					"tool_name", toolName,
					"args", args,
				)
				defer end()

				argsStr := map[string]string{}
				for k, v := range args {
					argsStr[k] = fmt.Sprintf("%v", v)
				}
				call := &taskengine.HookCall{
					Name:     hookName,
					ToolName: toolName,
					Args:     argsStr,
				}

				// `input` is nil here, but you can extend the JS signature later if needed.
				result, dataType, err := e.deps.HookRepo.Exec(
					ctx,
					time.Now().UTC(),
					nil,   // input
					false, // debug
					call,
				)
				if err != nil {
					if col != nil {
						col.Add(ExecLogEntry{
							Timestamp: time.Now().UTC(),
							Kind:      "executeHook",
							Name:      "executeHook",
							Error:     err.Error(),
							Meta: map[string]any{
								"hook_name": hookName,
								"tool_name": toolName,
							},
						})
					}
					return nil, fmt.Errorf("failed to execute hook %s/%s: %w", hookName, toolName, err)
				}

				// Normalize result for JS (similar to executeTaskChain)
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

				reportChange("hook_executed", map[string]any{
					"hook_name": hookName,
					"tool_name": toolName,
					"type":      dataType,
					"result":    jsResult,
				})

				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "executeHook",
						Name:      "executeHook",
						Meta: map[string]any{
							"hook_name": hookName,
							"tool_name": toolName,
						},
					})
				}

				return map[string]any{
					"success":   true,
					"hook_name": hookName,
					"tool_name": toolName,
					"type":      dataType,
					"result":    jsResult,
				}, nil
			})
		}); err != nil {
			return fmt.Errorf("failed to set executeHook builtin: %w", err)
		}
	}

	if err := setupHTTPFetch(vm, ctx, e.tracker, col, nil); err != nil {
		return err
	}

	return nil
}

func (e *Env) SetBuiltinHandlers(deps BuiltinHandlers) {
	if e == nil {
		return
	}
	e.deps = deps
}

// ExecOptions is a placeholder for future tuning (timeouts, maxSteps, etc).
type ExecOptions struct{}

// Compile wraps goja.Compile so callers don’t depend directly on goja.
func Compile(name, src string) (*goja.Program, error) {
	return goja.Compile(name, src, false)
}

// RunProgram executes a precompiled program in the given VM, with context
// cancellation and panic recovery. It DOES NOT wire builtins; caller must
// have called Env.SetupVM first.
func RunProgram(
	ctx context.Context,
	vm *goja.Runtime,
	prog *goja.Program,
	_ ExecOptions,
) (goja.Value, error) {
	if vm == nil {
		return goja.Undefined(), fmt.Errorf("vm is nil")
	}
	if prog == nil {
		return goja.Undefined(), fmt.Errorf("program is nil")
	}

	type res struct {
		val goja.Value
		err error
	}

	resultCh := make(chan res, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				var err error
				switch x := r.(type) {
				case string:
					err = fmt.Errorf("panic: %s", x)
				case error:
					err = x
				default:
					err = fmt.Errorf("panic: %v", x)
				}
				resultCh <- res{val: goja.Undefined(), err: err}
			}
		}()

		v, err := vm.RunProgram(prog)
		resultCh <- res{val: v, err: err}
	}()

	select {
	case <-ctx.Done():
		return goja.Undefined(), ctx.Err()
	case r := <-resultCh:
		return r.val, r.err
	}
}
