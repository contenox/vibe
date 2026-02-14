package localhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/vibe/jseval"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
	"github.com/dop251/goja"
	"github.com/getkin/kin-openapi/openapi3"
)

// Hook name used in chains: HookCall.Name: "js_sandbox"
const jsSandboxHookName = "js_sandbox"

// Tool name exposed to LLMs when using this hook as a tool.
const jsSandboxToolName = "execute_js"

// JSSandboxHook implements taskengine.HookRepo for executing generated JS
// code in a sandbox using the shared jseval.Env.
//
// Typical flow:
//
//  1. A task with handler "prompt_to_js" generates code:
//     output: { "code": "<javascript>" } (DataTypeJSON)
//
//  2. Next task uses handler "hook" with Hook.Name = "js_sandbox"
//     and passes the previous output as input.
//
//  3. This hook executes the code in a fresh goja VM, with:
//     - jseval.Env providing builtins (sendEvent, executeTask, etc.)
//     - Collector capturing console logs and builtin calls
//
//  4. The hook returns a JSON object:
//
//     {
//     "ok": true|false,
//     "error": "..." | null,
//     "result": <exported result or null>,
//     "logs": [ ExecLogEntry, ... ]
//     }
//
//     and DOES NOT treat script errors as hook errors, so the chain can
//     use this info as feedback to the LLM.
type JSSandboxHook struct {
	env     *jseval.Env
	tracker libtracker.ActivityTracker
}

var _ taskengine.HookRepo = (*JSSandboxHook)(nil)

// NewJSSandboxHook wires the shared jseval.Env into a HookRepo.
func NewJSSandboxHook(
	env *jseval.Env,
	tracker libtracker.ActivityTracker,
) taskengine.HookRepo {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &JSSandboxHook{
		env:     env,
		tracker: tracker,
	}
}

// Supports returns the single hook name this repo serves.
func (h *JSSandboxHook) Supports(ctx context.Context) ([]string, error) {
	return []string{jsSandboxHookName}, nil
}

// Exec runs the provided JS code in a sandbox and returns a structured result.
//
// Input conventions:
//   - If input is map[string]any and contains "code": string, that string is executed.
//   - If input is string, it is treated as JS source directly.
//
// Errors in the JS itself (compile/runtime) are reported in the returned JSON
// and DO NOT cause Exec to return an error, so the workflow can introspect and
// send a feedback loop back to the LLM.
func (h *JSSandboxHook) Exec(
	ctx context.Context,
	startingTime time.Time,
	input any,
	debug bool,
	hook *taskengine.HookCall,
) (any, taskengine.DataType, error) {
	reportErr, reportChange, end := h.tracker.Start(
		ctx,
		"exec",
		"js_sandbox",
		"hook_name", hook.Name,
	)
	defer end()

	if hook.Name != jsSandboxHookName {
		err := fmt.Errorf("unknown hook: %s (expected %s)", hook.Name, jsSandboxHookName)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}

	// ---- extract JS code from input ----
	var code string

	switch v := input.(type) {
	case map[string]any:
		if c, ok := v["code"]; ok {
			if s, ok := c.(string); ok {
				code = s
			}
		}
	case []byte:
		// maybe it's JSON with "code" field
		var m map[string]any
		if err := json.Unmarshal(v, &m); err == nil {
			if c, ok := m["code"].(string); ok {
				code = c
			}
		}
	case string:
		code = v
	default:
		// anything else is unexpected for this hook
	}

	code = strings.TrimSpace(code)
	if code == "" {
		err := fmt.Errorf("js_sandbox: empty or missing code in input")
		reportErr(err)
		// This is a structural error, not a script error -> surface as hook error
		return nil, taskengine.DataTypeAny, err
	}

	// ---- set up VM + collector ----
	vm := goja.New()

	collector := jseval.NewCollector()

	// Setup builtins + console / event / task functions with collector
	if err := h.env.SetupVM(ctx, vm, collector); err != nil {
		err = fmt.Errorf("js_sandbox: failed to setup VM: %w", err)
		reportErr(err)
		return nil, taskengine.DataTypeAny, err
	}

	// ---- compile & run ----
	// IMPORTANT: compile/runtime errors become part of the result object,
	// NOT hook-level errors, so the LLM can see them.
	var (
		execErr     error
		exportedRes any
	)

	prog, err := jseval.Compile("js_sandbox", code)
	if err != nil {
		// treat as script error
		execErr = fmt.Errorf("compile error: %w", err)
	} else {
		// run with context-aware execution
		_, runErr := jseval.RunProgram(ctx, vm, prog, jseval.ExecOptions{})
		if runErr != nil {
			execErr = fmt.Errorf("runtime error: %w", runErr)
		} else {
			// try to read global "result" if set
			val := vm.Get("result")
			if !goja.IsUndefined(val) && !goja.IsNull(val) {
				exportedRes = val.Export()
			}
		}
	}

	// collect logs
	logs := collector.Logs()

	// build response object
	resp := map[string]any{
		"ok":      execErr == nil,
		"code":    code,
		"result":  exportedRes,
		"logs":    logs,
		"started": startingTime,
		"ended":   time.Now().UTC(),
	}

	if execErr != nil {
		resp["error"] = execErr.Error()
	} else {
		resp["error"] = nil
	}

	// Track successful hook execution (even if JS had an error; that is part of resp)
	reportChange("js_sandbox", map[string]any{
		"ok":         execErr == nil,
		"has_result": exportedRes != nil,
		"logs_len":   len(logs),
	})

	return resp, taskengine.DataTypeJSON, nil
}

// GetSchemasForSupportedHooks returns OpenAPI schemas for supported hooks.
// For now we return an empty map (no full OpenAPI spec), which is acceptable.
func (h *JSSandboxHook) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// GetToolsForHookByName exposes the JS sandbox as a single function-tool
// ("execute_js") so models can call it directly when tools are wired
// via ExecuteConfig.Hooks / hook resolution.
func (h *JSSandboxHook) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != jsSandboxHookName {
		return nil, fmt.Errorf("unknown hook: %s", name)
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        jsSandboxToolName,
				Description: "Executes generated JavaScript in a sandbox and returns result + logs.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"code": map[string]any{
							"type":        "string",
							"description": "JavaScript source code to execute in the sandbox.",
						},
					},
					"required": []string{"code"},
				},
			},
		},
	}, nil
}
