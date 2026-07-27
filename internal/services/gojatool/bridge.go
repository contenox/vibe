package gojatool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/dop251/goja"
)

// HostToolCaller is the ONE seam through which sandboxed code reaches the world.
//
// It is deliberately a one-method interface owned by this package rather than an
// import of the engine's executor internals (the house style — see
// gointel.Index's consumers and localtools' FileIO): the sandbox needs "run this
// tool call and give me the result", nothing more, and depending on less means
// the whole toolset is testable with a five-line fake.
//
// The implementation the caller injects MUST be the engine's real tool path —
// the same aggregate repo the model's own tool calls travel through, HITL
// wrapper included. That is the one boundary rule: a script calling
// local_fs.write_file meets the same envelope a model call would, so script
// tools inherit the entire policy story for free. Wiring it to a raw,
// un-gated repo would silently turn every script into a policy bypass.
//
// provider is the tools-PROVIDER key ("local_fs", "gointel", an MCP server
// name); tool is the tool on it ("read_file"). args is plain JSON-decoded data.
// The returned value must be JSON-marshalable; it is what the script sees.
type HostToolCaller interface {
	CallTool(ctx context.Context, provider, tool string, args map[string]any) (any, error)
}

// HostFunc adapts a plain function to HostToolCaller.
type HostFunc func(ctx context.Context, provider, tool string, args map[string]any) (any, error)

func (f HostFunc) CallTool(ctx context.Context, provider, tool string, args map[string]any) (any, error) {
	return f(ctx, provider, tool, args)
}

// HostFromRepo adapts the engine's aggregate tools repo to HostToolCaller. It
// lives here rather than at the registration site so the exact ToolsCall shape a
// script produces is written and tested ONCE, in the package that has to be
// right about it.
//
// Pass the HITL-WRAPPED repo. That is the whole point of the one boundary rule:
// a script calling local_fs.write_file must meet the same envelope a model call
// would. (Wrapping it any deeper — outside the attention/tool-guidance
// decorator, say — would also feed script-internal calls into counters that are
// meant to observe MODEL-level navigation, so the HITL-wrapped repo is both the
// safe and the accurate seam.)
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
		// A DENIAL is not a result, and this is the one place that can tell the
		// difference. See IsDenyMessage.
		if IsDenyMessage(out) {
			return nil, fmt.Errorf("%w: %s.%s", ErrToolDenied, provider, tool)
		}
		return out, nil
	})
}

// denyResults are the exact strings the engine's HITL gate returns IN PLACE OF a
// tool result when a call is refused. They are mirrored here rather than
// imported because this package does not depend on the gate — the one boundary
// rule is about routing THROUGH it, not about knowing it. The registration
// site's e2e test pins these against the engine's own constants, so a drift
// fails loudly there instead of silently here.
var denyResults = []string{
	"User denied the operation. Please ask for clarification or try a different, less destructive approach.",
	"Approval timed out. The operation was automatically denied.",
}

// IsDenyMessage reports whether a host tool result is the envelope's REFUSAL
// rather than data.
//
// The distinction has to be drawn here because the gate answers a denial the way
// it answers a MODEL: with a sentence, returned as an ordinary successful result,
// on the assumption the reader will understand it and change course. A script
// cannot. It does `text.split("\n")` on that sentence and returns a confident
// wrong answer — the exact failure this package refuses everywhere else, arriving
// through the one door that looked like a success.
//
// So a denial becomes a thrown JS exception, the same treatment every other
// bridge refusal gets: a script that ignores it stops, and a script that means to
// continue without the call says so with try/catch. The policy decision is
// untouched — same gate, same verdict, delivered in a form the caller can act on.
//
// The residual false positive (a tool legitimately returning exactly this
// sentence, e.g. reading a file that quotes it) is accepted: refusing a call that
// was allowed costs a turn, while accepting a refusal as data costs the answer.
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

// SetHost binds (or rebinds) the host tool path.
//
// It exists because of an unavoidable construction cycle at the registration
// site: the aggregate tools repo the bridge must call is built FROM the map this
// toolset is registered in, so the toolset necessarily exists before the thing
// it calls. Late binding through one mutex-guarded setter is the smallest
// honest answer; the alternative — handing the sandbox a half-built repo — is
// the kind of cycle that turns into a nil deref six months later.
//
// Safe to call while executions are in flight: a script that calls host.tool
// mid-rebind sees either the old or the new caller, never a torn one.
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

// hostCallOptions is the third argument to host.tool: the author's explicit
// choices about how the result crosses back. It is deliberately tiny — one flag
// — because every option here is a way to opt OUT of a guard, and a guard with
// many doors is decoration.
type hostCallOptions struct {
	// raw returns the tool's value exactly as it came, so a text result arrives
	// as a bare JS string rather than a ToolText wrapper. See hostResult.
	raw bool
}

// hostOptionNames is the allowed key set of the options object, named in the
// refusal so a typo costs a line rather than a debugging session.
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

// installHost defines the `host` global. There is exactly one method on it, and
// exactly one thing it can do.
//
// Refusals are thrown as ordinary JS exceptions rather than returned as values:
// a script that ignores a refused call and carries on with `undefined` produces
// a wrong answer quietly, which is the failure mode this whole package exists to
// avoid. A script that WANTS to continue can try/catch, which is an explicit
// decision by its author.
func (s *sandbox) installHost(ctx context.Context, vm *goja.Runtime, codec jsonCodec, enabled bool, label string, hs *hostState) error {
	obj := vm.NewObject()

	// refuse throws msg into JS and records the sentinel, so an UNCAUGHT refusal
	// still satisfies errors.Is on the way out (see sandbox.execError). A script
	// that catches it has made an explicit decision and gets no sentinel.
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
		// The recursion guard. Depth is a structural rule, checked BEFORE anything
		// about the host: it does not depend on what happens to be wired. A script
		// may reach the world, but it may not reach BACK into this sandbox. The
		// assertion is the provider prefix on the call, which is the only name the
		// guard can trust — the engine addresses every tool as "<provider>.<tool>",
		// so a goja tool cannot be reached under any other spelling.
		refuseRecursion := func() {
			refuse(ErrRecursionRefused, fmt.Sprintf(
				"%s: %s tried to call %s, but scripts may not invoke %q-provider tools — sandbox depth is exactly one. Inline the computation you wanted to delegate; you are already inside the sandbox.",
				ErrRecursionRefused, label, echoArg(name), ToolsProviderName))
		}

		provider, tool, err := splitToolName(name)
		if err != nil {
			// An unqualified attempt at this provider's own tools gets the real
			// reason rather than a lecture about the address form, which would
			// cost a turn to learn and a second turn to be refused anyway.
			if name == ToolsProviderName || name == ToolEval || strings.HasPrefix(name, ToolsProviderName+"_") {
				refuseRecursion()
			}
			panic(jsError(vm, err.Error()))
		}
		if provider == ToolsProviderName {
			refuseRecursion()
		}

		// THE DECLARED REACH. A script that listed its tools may call those and
		// nothing else. Checked here, beside the recursion guard, because it is
		// the same kind of rule: a property of THIS execution, settled before
		// anything about the host is consulted.
		//
		// It is defence in depth, not the policy boundary — the envelope still
		// evaluates every call that gets past it. What it adds is the thing the
		// envelope structurally cannot give: a statement of reach that exists
		// BEFORE the call, which is what lets an approval card for a script say
		// what the script will touch instead of "unknown".
		if reach := hs.reach; reach != nil && !reach.permits(name) {
			refuse(ErrToolUndeclared, reach.refusal(label, name))
		}

		// The budget on reaching the world. Checked before the host is consulted,
		// for the same reason the recursion guard is: it is a structural rule about
		// this execution, not about what happens to be wired.
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

		// The deadline clock stops for the duration of this call: what is on the
		// other side may be an approval card with a human in front of it.
		result, cerr := hs.stopClock(func() (any, error) {
			return host.CallTool(ctx, provider, tool, args)
		})
		if cerr != nil {
			// A refusal by the approval envelope keeps its sentinel, so an
			// UNCAUGHT denial still satisfies errors.Is on the way out and the
			// message says plainly that no result exists.
			if errors.Is(cerr, ErrToolDenied) {
				refuse(ErrToolDenied, fmt.Sprintf(
					"%s: %s.%s was refused by the approval envelope, so it produced no result — the gate answers a denial with a sentence meant for a human, which is not data this script can use. If %s should carry on without that call, wrap it in try/catch.",
					ErrToolDenied, provider, tool, label))
			}
			// Otherwise the host's message is preserved verbatim: it is a teaching
			// error written for exactly this moment (a denied path, a missing file,
			// an unknown argument), and paraphrasing it would throw that away.
			panic(jsError(vm, clampText(cerr.Error(), maxErrorTextBytes)))
		}

		value, jerr := hostResult(codec, provider, tool, opts.raw, result)
		if jerr != nil {
			// A stand-in with no program-facing meaning is a refusal like any
			// other guard's: it keeps its sentinel so an uncaught one still
			// satisfies errors.Is at the exec boundary.
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

// splitToolName parses the "<provider>.<tool>" address. The dotted form is not a
// convenience: it is the SAME spelling the model sees in its tool list and the
// same one a policy rule addresses, so a script author reading a transcript can
// copy a call verbatim.
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

// hostArgs converts the JS arguments object into plain Go data through JSON, so
// nothing live (a function, a closure, a JS object with getters) can cross into
// the engine's tool path.
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

// jsError builds a JS Error object to panic with. goja converts a panicked Value
// into a catchable JS exception (vm.handleThrow), which is how a Go-side refusal
// becomes `catch (e) { e.message }` on the script side with the message intact.
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

// declaredReach is a script's `tools: [...]` declaration, resolved once at load.
//
// A nil *declaredReach means the script declared NOTHING, which is unrestricted:
// the field is optional so every script written before it existed keeps working,
// and the loader says once, at startup, which scripts are in that state. An
// EMPTY declaration is not the same thing — `tools: []` says "this script
// reaches nothing", and is enforced as written.
type declaredReach struct {
	// file is the script the declaration lives in; the refusal names it because
	// the repair is an edit to that file.
	file string
	// names is the declared set, in declaration order for the message and as a
	// set for the check.
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

// refusal is the teaching error for an undeclared call: it names the address
// that was refused, what the script DID declare, and the exact edit.
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
// single-threaded by construction), so it needs no lock; the two channels are
// the whole synchronisation with the watchdog goroutine.
type hostState struct {
	// reach is the declared tool allowlist for this execution, or nil for an
	// unrestricted one (goja_eval, and any script that declares no `tools`).
	reach *declaredReach

	// refusal is the sentinel for the last guard this bridge tripped. It exists
	// because a refusal leaves Go, becomes a JS exception, and comes back as a
	// *goja.Exception — a form errors.Is can no longer see through.
	refusal error

	// paused/resumed bracket every host.tool call, stopping and restarting the
	// deadline clock. See stopClock for why the deadline must not run there.
	paused  chan struct{}
	resumed chan struct{}

	// calls counts host.tool dispatches, bounded by maxHostCalls. Executing
	// goroutine only.
	calls int

	// hostWait is the total time this execution spent parked in host.tool. It is
	// written and read on the executing goroutine only. It exists so a deadline
	// error can report COMPUTE time rather than wall time — "ran for 4m12s and
	// was interrupted at its 2s deadline" is a sentence that teaches the wrong
	// lesson when four of those minutes were an operator reading a card.
	hostWait time.Duration
}

func newHostState() *hostState {
	// Buffered so the bridge's send always lands even if the watchdog is
	// momentarily between selects; the watchdog picks it up on its next turn and
	// the pause/resume pair still nets out correctly.
	return &hostState{
		paused:  make(chan struct{}, 1),
		resumed: make(chan struct{}, 1),
	}
}

// stopClock pauses the execution deadline for the duration of fn, which is one
// host.tool call.
//
// THE DEADLINE BOUNDS COMPUTE, NOT WAITING. This is the correction to an
// assumption that looked right and was not: the watchdog originally ran straight
// through a host.tool call on the reasoning that a script must not extend its
// budget by calling out to the world in a loop. But the thing on the other side
// of that call is the approval envelope, and an approve-tier tool means A HUMAN
// READING A CARD. No human answers in 2 seconds, so every script that reached a
// gated tool died at its own deadline the moment the operator clicked allow —
// the headline capability, unusable, and unit-testable nowhere because no unit
// test has a human in it. Found by using it (2026-07-27).
//
// Nothing is lost by pausing. The wait is already bounded from three directions
// that all still fire here: the approval machinery's own park window and rule
// timeout, the caller's context, and Shutdown — each of which interrupts the VM
// through the same watchdog. What the deadline goes back to bounding is the only
// thing it can honestly bound: time this sandbox spends executing.
func (hs *hostState) stopClock(fn func() (any, error)) (any, error) {
	if hs == nil || hs.paused == nil {
		return fn()
	}
	// A watchdog that has already fired, or already returned, leaves nobody to
	// receive: the send is best-effort in both directions so the call proceeds
	// either way (a stuck bridge would be far worse than an unpaused clock).
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
