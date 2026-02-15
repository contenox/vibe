package jseval

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/eventsourceservice"
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
	tracker  libtracker.ActivityTracker
	deps     BuiltinHandlers
	builtins []Builtin
}

func NewEnv(
	tracker libtracker.ActivityTracker,
	deps BuiltinHandlers,
	builtins []Builtin,
) *Env {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &Env{
		tracker:  tracker,
		deps:    deps,
		builtins: builtins,
	}
}

// GetBuiltinSignatures returns tool-shaped descriptions for all registered builtins,
// for use in sandbox API documentation to the model.
func (e *Env) GetBuiltinSignatures() []taskengine.Tool {
	if e == nil || len(e.builtins) == 0 {
		return nil
	}
	out := make([]taskengine.Tool, 0, len(e.builtins))
	for _, b := range e.builtins {
		out = append(out, taskengine.Tool{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        b.Name(),
				Description: b.Description(),
				Parameters:  b.ParametersSchema(),
			},
		})
	}
	return out
}

// GetExecuteHookToolDescriptions returns tool-shaped descriptions for all tools
// callable via executeHook(hookName, toolName, args), using the env's HookRepo.
// Used to document the sandbox API for the model.
func (e *Env) GetExecuteHookToolDescriptions(ctx context.Context) ([]taskengine.Tool, error) {
	if e == nil || e.deps.HookRepo == nil {
		return nil, nil
	}
	supported, err := e.deps.HookRepo.Supports(ctx)
	if err != nil || len(supported) == 0 {
		return nil, err
	}
	var out []taskengine.Tool
	for _, hookName := range supported {
		if hookName == "js_execution" {
			continue // avoid recursion: js_execution's GetToolsForHookByName calls us
		}
		tools, err := e.deps.HookRepo.GetToolsForHookByName(ctx, hookName)
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			name := "executeHook:" + hookName + "." + t.Function.Name
			out = append(out, taskengine.Tool{
				Type: "function",
				Function: taskengine.FunctionTool{
					Name:        name,
					Description: "Call executeHook(\"" + hookName + "\", \"" + t.Function.Name + "\", args). " + t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
	}
	return out, nil
}

// SetupVM wires console + builtins into the given VM using the env’s deps.
// Everything they do gets appended to the Collector.
func (e *Env) SetupVM(ctx context.Context, vm *goja.Runtime, col *Collector) error {
	if vm == nil {
		return fmt.Errorf("vm is nil")
	}

	if len(e.builtins) > 0 {
		for _, b := range e.builtins {
			if err := b.Register(vm, ctx, e.tracker, col, e.deps); err != nil {
				return fmt.Errorf("failed to register builtin %s: %w", b.Name(), err)
			}
		}
		return nil
	}

	// Legacy path: no builtins slice, wire inline (backward compatibility).
	if err := setupConsoleLogger(vm, ctx, e.tracker, col); err != nil {
		reportErr, _, end := e.tracker.Start(ctx, "setup", "console_logger_error", "err", err.Error())
		defer end()
		reportErr(err)
	}
	if e.deps.Eventsource != nil {
		if err := (SendEventBuiltin{}).Register(vm, ctx, e.tracker, col, e.deps); err != nil {
			return err
		}
	}

	if e.deps.TaskchainService != nil {
		if err := (CallTaskChainBuiltin{}).Register(vm, ctx, e.tracker, col, e.deps); err != nil {
			return err
		}
	}

	if e.deps.TaskService != nil {
		if err := (ExecuteTaskBuiltin{}).Register(vm, ctx, e.tracker, col, e.deps); err != nil {
			return err
		}
	}

	if e.deps.TaskchainService != nil && e.deps.TaskchainExecService != nil {
		if err := (ExecuteTaskChainBuiltin{}).Register(vm, ctx, e.tracker, col, e.deps); err != nil {
			return err
		}
	}

	if e.deps.HookRepo != nil {
		if err := (ExecuteHookBuiltin{}).Register(vm, ctx, e.tracker, col, e.deps); err != nil {
			return err
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
