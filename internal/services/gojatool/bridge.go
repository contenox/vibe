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

// HostToolCaller is the one seam through which sandboxed code reaches the
// world. The implementation injected must be the engine's real, HITL-wrapped
// tool path. provider.tool addresses the call; args and the return value are
// plain JSON-marshalable data.
type HostToolCaller interface {
	CallTool(ctx context.Context, provider, tool string, args map[string]any) (any, error)
}

// HostFunc adapts a plain function to HostToolCaller.
type HostFunc func(ctx context.Context, provider, tool string, args map[string]any) (any, error)

func (f HostFunc) CallTool(ctx context.Context, provider, tool string, args map[string]any) (any, error) {
	return f(ctx, provider, tool, args)
}

// HostFromRepo adapts the engine's aggregate tools repo to HostToolCaller.
// Pass the HITL-wrapped repo: a script calling local_fs.write_file must meet
// the same envelope a model call would.
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

// denyResults are the exact strings the engine's HITL gate returns in place
// of a tool result when a call is refused. Mirrored here since this package
// does not import the gate; the registration site's e2e test pins these
// against the engine's own constants.
var denyResults = []string{
	"User denied the operation. Please ask for clarification or try a different, less destructive approach.",
	"Approval timed out. The operation was automatically denied.",
}

// IsDenyMessage reports whether a host tool result is the envelope's refusal
// rather than data, so it can be thrown instead of handed to a script as
// content. A tool legitimately returning this exact sentence is an accepted
// false positive.
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

// SetHost binds (or rebinds) the host tool path. Safe to call while
// executions are in flight: a script that calls host.tool mid-rebind sees
// either the old or the new caller, never a torn one.
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

// hostToolUsage is the one-line teaching text every bridge refusal ends with.
const hostToolUsage = `Call it as host.tool("provider.tool_name", {arg: value}) — for example host.tool("local_fs.read_file", {path: "README.md"}).`

// hostCallOptions is the third argument to host.tool: explicit opt-outs from
// a guard, kept deliberately small.
type hostCallOptions struct {
	// raw returns the tool's value exactly as it came, so a text result
	// arrives as a bare JS string rather than a ToolText wrapper.
	raw bool
}

// hostOptionNames is the allowed key set of the options object, named in the
// refusal.
var hostOptionNames = []string{"raw"}

// hostOptions decodes the optional third argument.
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

// installHost defines the `host` global, with exactly one method on it.
// Refusals are thrown as ordinary JS exceptions rather than returned as
// values, so a script that ignores a refused call cannot silently continue
// with `undefined`; a script that wants to continue can try/catch.
func (s *sandbox) installHost(ctx context.Context, vm *goja.Runtime, codec jsonCodec, enabled bool, label string, hs *hostState) error {
	obj := vm.NewObject()

	// refuse throws msg into JS and records the sentinel, so an uncaught
	// refusal still satisfies errors.Is on the way out (see execError).
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
		// The recursion guard: a script may reach the world but not reach back
		// into this sandbox. Checked by provider prefix, since the engine
		// addresses every tool as "<provider>.<tool>".
		refuseRecursion := func() {
			refuse(ErrRecursionRefused, fmt.Sprintf(
				"%s: %s tried to call %s, but scripts may not invoke %q-provider tools — sandbox depth is exactly one. Inline the computation you wanted to delegate; you are already inside the sandbox.",
				ErrRecursionRefused, label, echoArg(name), ToolsProviderName))
		}

		provider, tool, err := splitToolName(name)
		if err != nil {
			// An unqualified attempt at this provider's own tools gets the
			// real reason rather than a lecture about the address form.
			if name == ToolsProviderName || name == ToolEval || strings.HasPrefix(name, ToolsProviderName+"_") {
				refuseRecursion()
			}
			panic(jsError(vm, err.Error()))
		}
		if provider == ToolsProviderName {
			refuseRecursion()
		}

		// The declared reach: a script that listed its tools may call those
		// and nothing else. Defence in depth, not the policy boundary — the
		// envelope still evaluates every call that gets past it.
		if reach := hs.reach; reach != nil && !reach.permits(name) {
			refuse(ErrToolUndeclared, reach.refusal(label, name))
		}

		// The budget on reaching the world, checked before the host is
		// consulted.
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

		// The deadline clock stops for the duration of this call: what is on
		// the other side may be an approval card with a human in front of it.
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
			// Otherwise the host's message is preserved verbatim.
			panic(jsError(vm, clampText(cerr.Error(), maxErrorTextBytes)))
		}

		value, jerr := hostResult(codec, provider, tool, opts.raw, result)
		if jerr != nil {
			// A stand-in with no program-facing meaning is a refusal like any
			// other guard's: it keeps its sentinel.
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

// splitToolName parses the "<provider>.<tool>" address, the same spelling the
// model sees in its tool list.
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

// hostArgs converts the JS arguments object into plain Go data through JSON,
// so nothing live (a function, a closure, a getter) can cross into the
// engine's tool path.
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

// jsError builds a JS Error object to panic with; goja converts a panicked
// Value into a catchable JS exception.
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

// declaredReach is a script's `tools: [...]` declaration, resolved once at
// load. A nil *declaredReach means the script declared nothing, which is
// unrestricted; an empty declaration (`tools: []`) means the script reaches
// nothing and is enforced as written.
type declaredReach struct {
	// file is the script the declaration lives in; the refusal names it.
	file string
	// names is the declared set, in declaration order for the message and as
	// a set for the check.
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

// refusal is the teaching error for an undeclared call.
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

// hostState carries what the bridge learned during one execution back to the
// exec boundary, and carries the deadline clock's pause signal out to the
// watchdog. Only the executing goroutine touches `refusal` (the VM is
// single-threaded by construction), so it needs no lock.
type hostState struct {
	// reach is the declared tool allowlist for this execution, or nil for an
	// unrestricted one.
	reach *declaredReach

	// refusal is the sentinel for the last guard this bridge tripped. It
	// exists because a refusal leaves Go, becomes a JS exception, and comes
	// back as a *goja.Exception, a form errors.Is can no longer see through.
	refusal error

	// paused/resumed bracket every host.tool call, stopping and restarting
	// the deadline clock. See stopClock.
	paused  chan struct{}
	resumed chan struct{}

	// calls counts host.tool dispatches, bounded by maxHostCalls. Executing
	// goroutine only.
	calls int

	// hostWait is the total time this execution spent parked in host.tool,
	// written and read on the executing goroutine only. Lets a deadline error
	// report compute time rather than wall time.
	hostWait time.Duration
}

func newHostState() *hostState {
	// Buffered so the bridge's send always lands even if the watchdog is
	// momentarily between selects.
	return &hostState{
		paused:  make(chan struct{}, 1),
		resumed: make(chan struct{}, 1),
	}
}

// stopClock pauses the execution deadline for the duration of fn, which is
// one host.tool call — the deadline bounds compute, not waiting on a human
// approval. The wait remains bounded by the approval machinery's own timeout,
// the caller's context, and Shutdown, each of which still interrupts the VM
// through the same watchdog.
func (hs *hostState) stopClock(fn func() (any, error)) (any, error) {
	if hs == nil || hs.paused == nil {
		return fn()
	}
	// Best-effort in both directions: a watchdog that has already fired or
	// returned leaves nobody to receive, and a stuck bridge would be worse
	// than an unpaused clock.
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
