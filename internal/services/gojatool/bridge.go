package gojatool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/dop251/goja"
)

// HostToolCaller is the seam through which sandboxed code reaches the world; the implementation injected must be the engine's HITL-wrapped tool path, addressed as provider.tool with JSON-marshalable args and return value.
type HostToolCaller interface {
	CallTool(ctx context.Context, provider, tool string, args map[string]any) (any, error)
}

// HostFunc adapts a plain function to HostToolCaller.
type HostFunc func(ctx context.Context, provider, tool string, args map[string]any) (any, error)

func (f HostFunc) CallTool(ctx context.Context, provider, tool string, args map[string]any) (any, error) {
	return f(ctx, provider, tool, args)
}

// HostFromRepo adapts the engine's aggregate tools repo to HostToolCaller; pass the HITL-wrapped repo so a script call meets the same envelope a model call would.
func HostFromRepo(repo taskengine.ToolsRepo) HostToolCaller {
	if repo == nil {
		return nil
	}
	return HostFunc(func(ctx context.Context, provider, tool string, args map[string]any) (any, error) {
		out, _, err := repo.Exec(ctx, time.Now().UTC(), args, false, &taskengine.ToolsCall{
			Name:     provider,
			ToolName: tool,
		})
		if err != nil {
			return nil, err
		}
		// A denial is not a result. See IsDenyMessage.
		if IsDenyMessage(out) {
			return nil, fmt.Errorf("%w: %s.%s", ErrToolDenied, provider, tool)
		}
		return out, nil
	})
}

var denyResults = []string{
	"User denied the operation. Please ask for clarification or try a different, less destructive approach.",
	"Approval timed out. The operation was automatically denied.",
}

// IsDenyMessage reports whether a host tool result is the envelope's refusal rather than data; a tool returning this exact sentence is an accepted false positive.
func IsDenyMessage(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	s = strings.TrimSpace(s)
	for _, deny := range denyResults {
		if s == deny {
			return true
		}
	}
	return false
}

// SetHost binds (or rebinds) the host tool path; safe to call while executions are in flight, since a mid-rebind call sees either the old or the new caller, never a torn one.
func (t *Toolset) SetHost(h HostToolCaller) {
	t.sb.hostMu.Lock()
	t.sb.host = h
	t.sb.hostMu.Unlock()
}

func (s *sandbox) currentHost() HostToolCaller {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	return s.host
}

const hostToolUsage = `Call it as host.tool("provider.tool_name", {arg: value}) — for example host.tool("local_fs.read_file", {path: "README.md"}).`

type hostCallOptions struct {
	raw bool
}

var hostOptionNames = []string{"raw"}

func hostOptions(codec jsonCodec, fc goja.FunctionCall) (hostCallOptions, error) {
	var opts hostCallOptions
	if len(fc.Arguments) < 3 || goja.IsUndefined(fc.Arguments[2]) || goja.IsNull(fc.Arguments[2]) {
		return opts, nil
	}
	raw, err := codec.marshal(fc.Arguments[2])
	if err != nil {
		return opts, fmt.Errorf("goja: host.tool options must be a plain object like {raw: true}: %s", withoutSeverity(err.Error()))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return opts, fmt.Errorf("goja: host.tool options must be an object like {raw: true}, got %s", clampText(string(raw), 120))
	}
	for key, value := range decoded {
		switch key {
		case "raw":
			b, ok := value.(bool)
			if !ok {
				return opts, fmt.Errorf("goja: host.tool option raw must be true or false, got %s", clampText(fmt.Sprint(value), 64))
			}
			opts.raw = b
		default:
			return opts, fmt.Errorf("goja: host.tool: unknown option %s (allowed: %s)", echoName(key), strings.Join(hostOptionNames, ", "))
		}
	}
	return opts, nil
}

func (s *sandbox) installHost(ctx context.Context, vm *goja.Runtime, codec jsonCodec, enabled bool, label string, hs *hostState) error {
	obj := vm.NewObject()

	// refuse's sentinel lets errors.Is see through the JS exception boundary (see execError).
	refuse := func(sentinel error, msg string) {
		hs.refusal = sentinel
		panic(jsError(vm, msg))
	}

	call := func(fc goja.FunctionCall) goja.Value {
		if !enabled {
			panic(jsError(vm, "goja: host.tool is not available while a script is being loaded — the file's top level runs before its tool exists. Move the call inside run(args)."))
		}

		name := ""
		if len(fc.Arguments) > 0 && !goja.IsUndefined(fc.Arguments[0]) && !goja.IsNull(fc.Arguments[0]) {
			name = strings.TrimSpace(fc.Arguments[0].String())
		}
		// Recursion guard: sandbox depth is exactly one, checked by provider prefix ("<provider>.<tool>").
		refuseRecursion := func() {
			refuse(ErrRecursionRefused, fmt.Sprintf(
				"%s: %s tried to call %s, but scripts may not invoke %q-provider tools — sandbox depth is exactly one. Inline the computation you wanted to delegate; you are already inside the sandbox.",
				ErrRecursionRefused, label, echoArg(name), ToolsProviderName))
		}

		provider, tool, err := splitToolName(name)
		if err != nil {
			if name == ToolsProviderName || name == ToolEval || strings.HasPrefix(name, ToolsProviderName+"_") {
				refuseRecursion()
			}
			panic(jsError(vm, err.Error()))
		}
		if provider == ToolsProviderName {
			refuseRecursion()
		}

		// Declared reach is defence in depth, not the policy boundary — the envelope still evaluates every call that passes it.
		if reach := hs.reach; reach != nil && !reach.permits(name) {
			refuse(ErrToolUndeclared, reach.refusal(label, name))
		}

		// Budget checked before the host is consulted.
		if hs.calls >= maxHostCalls {
			refuse(ErrHostBudget, fmt.Sprintf(
				"%s: %s has already made %d host.tool calls, the limit for one execution. A tool call that needs to reach the world more times than this is a pipeline, not a tool — fetch less, or split the work across separate calls.",
				ErrHostBudget, label, maxHostCalls))
		}
		hs.calls++

		host := s.currentHost()
		if host == nil {
			refuse(ErrHostUnavailable, fmt.Sprintf("%s: no tool path is wired into this sandbox, so scripts can only compute. %s", ErrHostUnavailable, hostToolUsage))
		}

		args, err := hostArgs(codec, fc)
		if err != nil {
			panic(jsError(vm, err.Error()))
		}
		opts, oerr := hostOptions(codec, fc)
		if oerr != nil {
			panic(jsError(vm, oerr.Error()))
		}

		// The deadline clock stops for this call: the other side may be a human-gated approval.
		result, cerr := hs.stopClock(func() (any, error) {
			return host.CallTool(ctx, provider, tool, args)
		})
		if cerr != nil {
			// A refusal by the approval envelope keeps its sentinel.
			if errors.Is(cerr, ErrToolDenied) {
				refuse(ErrToolDenied, fmt.Sprintf(
					"%s: %s.%s was refused by the approval envelope, so it produced no result — the gate answers a denial with a sentence meant for a human, which is not data this script can use. If %s should carry on without that call, wrap it in try/catch.",
					ErrToolDenied, provider, tool, label))
			}
			panic(jsError(vm, clampText(cerr.Error(), maxErrorTextBytes)))
		}

		value, jerr := hostResult(codec, provider, tool, opts.raw, result)
		if jerr != nil {
			// A stand-in with no program-facing meaning refuses like any other guard, keeping its sentinel.
			var unusable *unusableResultError
			if errors.As(jerr, &unusable) {
				refuse(ErrToolNotData, unusable.Error())
			}
			panic(jsError(vm, fmt.Sprintf("goja: %s.%s returned a value that is not JSON: %s", provider, tool, clampText(jerr.Error(), maxErrorTextBytes))))
		}
		return value
	}

	if err := obj.Set("tool", call); err != nil {
		return err
	}
	return vm.Set("host", obj)
}

func splitToolName(name string) (provider, tool string, err error) {
	if name == "" {
		return "", "", fmt.Errorf("goja: host.tool needs a tool name. %s", hostToolUsage)
	}
	idx := strings.Index(name, ".")
	if idx <= 0 || idx == len(name)-1 {
		return "", "", fmt.Errorf("goja: host.tool(%s) is not a tool address — it needs both a provider and a tool. %s", echoArg(name), hostToolUsage)
	}
	return name[:idx], name[idx+1:], nil
}

func hostArgs(codec jsonCodec, fc goja.FunctionCall) (map[string]any, error) {
	if len(fc.Arguments) < 2 || goja.IsUndefined(fc.Arguments[1]) || goja.IsNull(fc.Arguments[1]) {
		return map[string]any{}, nil
	}
	raw, err := codec.marshal(fc.Arguments[1])
	if err != nil {
		return nil, fmt.Errorf("goja: host.tool arguments must be plain JSON data: %s. %s", withoutSeverity(err.Error()), hostToolUsage)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("goja: host.tool arguments must be an object, got %s. %s", clampText(string(raw), 120), hostToolUsage)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func jsError(vm *goja.Runtime, msg string) goja.Value {
	ctor := vm.Get("Error")
	if ctor == nil {
		return vm.ToValue(msg)
	}
	obj, err := vm.New(ctor, vm.ToValue(msg))
	if err != nil {
		return vm.ToValue(msg)
	}
	return obj
}

type declaredReach struct {
	file  string
	names []string
	set   map[string]struct{}
}

func newDeclaredReach(file string, names []string) *declaredReach {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return &declaredReach{file: file, names: names, set: set}
}

func (d *declaredReach) permits(name string) bool {
	_, ok := d.set[name]
	return ok
}

func (d *declaredReach) refusal(label, name string) string {
	declared := "tools: [] (this script declares that it reaches nothing)"
	if len(d.names) > 0 {
		declared = "tools: [" + strings.Join(quoteAll(d.names), ", ") + "]"
	}
	return fmt.Sprintf(
		"%s: %s tried to call %s, which it does not declare. Its descriptor says %s, and every host.tool address a script uses must be listed there — that declaration is what an approval card shows an operator BEFORE the script runs. Add %s to the tools array in %s if this script is meant to reach it.",
		ErrToolUndeclared, label, echoArg(name), declared, echoArg(name), d.file)
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

type hostState struct {
	reach *declaredReach

	refusal error

	paused  chan struct{}
	resumed chan struct{}

	calls int

	hostWait time.Duration
}

func newHostState() *hostState {
	// Buffered so the bridge's send always lands even when the watchdog is between selects.
	return &hostState{
		paused:  make(chan struct{}, 1),
		resumed: make(chan struct{}, 1),
	}
}

func (hs *hostState) stopClock(fn func() (any, error)) (any, error) {
	if hs == nil || hs.paused == nil {
		return fn()
	}
	// Best-effort: a fired/returned watchdog leaves nobody to receive, and blocking would be worse than an unpaused clock.
	paused := false
	select {
	case hs.paused <- struct{}{}:
		paused = true
	default:
	}
	started := time.Now()
	out, err := fn()
	hs.hostWait += time.Since(started)
	if paused {
		select {
		case hs.resumed <- struct{}{}:
		default:
		}
	}
	return out, err
}
