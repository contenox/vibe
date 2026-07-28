// Package gojatool is the agent's embedded ECMAScript sandbox: script tools
// (operator *.js files) and goja_eval (model-driven bounded compute) on one
// goja runtime. Scripts have no ambient I/O; their only reach into the world
// is host.tool, routed through the same tool path and HITL as any other call.
package gojatool

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// --- Limits -----------------------------------------------------------------
// Each limit below is asserted by a test in sandbox_test.go.

const (
	// DefaultDeadline bounds a single execution. goja has no memory cap, so
	// this is also the de-facto memory bound: an allocation bomb grows the
	// heap at roughly 150-200 MB/s until the deadline interrupts it.
	DefaultDeadline = 2 * time.Second

	// MaxDeadline is the ceiling on any per-call or per-script deadline
	// override.
	MaxDeadline = 30 * time.Second

	// DefaultOutputCap bounds the marshaled JSON a single execution returns,
	// not what the script allocates while producing it.
	DefaultOutputCap = 64 << 10
)

const (
	// minDeadline floors any override.
	minDeadline = 10 * time.Millisecond

	// maxOutputCap ceilings a configured OutputCap.
	maxOutputCap = 1 << 20

	// defaultMaxCallStack is the goja SetMaxCallStackSize value; goja's own
	// default is unbounded.
	defaultMaxCallStack = 512

	// maxSourceBytes bounds one script file and one goja_eval `code` argument.
	// Compilation is native Go work that the interrupt cannot preempt.
	maxSourceBytes = 128 << 10

	// programCacheEntries bounds the compiled-program LRU, keyed by source
	// SHA-256.
	programCacheEntries = 64

	// Console capture bounds: console.log appends to a buffer returned with
	// the result rather than performing ambient I/O.
	maxLogLines     = 100
	maxLogLineBytes = 1 << 10
	maxLogBytes     = 8 << 10

	// maxHostCalls bounds how many times one execution may reach the world
	// through host.tool. The deadline stops while a host call is in flight
	// (see hostState.stopClock), so it cannot bound a host.tool loop; this
	// limit does instead.
	maxHostCalls = 256

	// maxErrorTextBytes clamps a model-facing error built from script-
	// controlled text (a thrown message, a JS stack).
	maxErrorTextBytes = 2 << 10

	// maxEchoRunes bounds how much of a model-supplied string a teaching
	// error quotes back.
	maxEchoRunes = 120

	// shutdownGrace bounds how long Shutdown waits for in-flight executions:
	// an in-flight host.tool call may be parked on a human approval, and
	// teardown must not hang on that.
	shutdownGrace = 5 * time.Second
)

// --- Errors -----------------------------------------------------------------
// Every error here is recoverable by a corrected call, so none is marked fatal.

const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// Sentinel errors, for errors.Is by callers and tests.
var (
	// ErrDeadline means the execution was interrupted at its deadline without
	// producing a value.
	ErrDeadline = errors.New("goja: deadline exceeded")
	// ErrShutdown means the toolset has been shut down and will run nothing
	// further.
	ErrShutdown = errors.New("goja: sandbox shut down")
	// ErrHostUnavailable means host.tool was called but no HostToolCaller is
	// wired.
	ErrHostUnavailable = errors.New("goja: host tool path unavailable")
	// ErrRecursionRefused means a script tried to invoke a goja-provider tool;
	// depth is exactly one, by design.
	ErrRecursionRefused = errors.New("goja: recursive goja tool call refused")
	// ErrToolDenied means a host.tool call was refused by the approval
	// envelope. See IsDenyMessage.
	ErrToolDenied = errors.New("goja: host tool call denied")
	// ErrToolUndeclared means a script called a tool its descriptor does not
	// list in `tools: [...]`. See declaredReach.
	ErrToolUndeclared = errors.New("goja: host tool not declared by this script")
	// ErrToolNotData means a tool answered with a stand-in that has no
	// meaning for a program — a stub, a notice, a refusal. See hostresult.go.
	ErrToolNotData = errors.New("goja: host tool result is not data")
	// ErrHostBudget means one execution exhausted its host.tool budget
	// (maxHostCalls).
	ErrHostBudget = errors.New("goja: host tool budget exhausted")
	// ErrScriptLoad means a script file failed fail-fast validation at load.
	ErrScriptLoad = errors.New("goja: script load failed")
	// ErrNotJSON means a value could not cross the JSON-only boundary.
	ErrNotJSON = errors.New("goja: value is not JSON-representable")
)

// recoverablef builds a teaching error tagged recoverable-by-correction.
func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

// wrapRecoverable tags a sentinel-wrapping error recoverable unless it
// already carries a severity marker.
func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

// markSeverity tags err recoverable-by-correction unless already tagged,
// preserving the error chain while appending the marker to the rendered text.
func markSeverity(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return err
	}
	return fmt.Errorf("%w %s", err, severityRecoverable)
}

// withoutSeverity strips a severity marker from text about to be embedded in
// another error, so a nested message never carries two markers.
func withoutSeverity(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, severityRecoverable, ""))
}

// clampText bounds script- or model-controlled text embedded in an error.
func clampText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := truncateAtRune(s[:max])
	return cut + fmt.Sprintf("… (+%d more bytes)", len(s)-len(cut))
}

// echoArg renders a model-supplied argument for an error: clamped, then
// Go-quoted so control characters, NULs and bidi overrides are escaped.
func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return fmt.Sprintf("%q… (+%d more characters)", string(r[:maxEchoRunes]), len(r)-maxEchoRunes)
	}
	return fmt.Sprintf("%q", s)
}

// truncateAtRune backs a byte-truncated string off to the last complete rune,
// so a cap never emits half a code point.
func truncateAtRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// --- Result -----------------------------------------------------------------

// Result is what every goja tool returns to the engine (DataTypeJSON).
type Result struct {
	// Value is the script's return value, marshaled as JSON. On truncation it
	// becomes a JSON string holding the head of the original plus Notice.
	Value json.RawMessage `json:"value"`
	// Truncated reports that Value hit the output cap.
	Truncated bool `json:"truncated,omitempty"`
	// Notice is the explicit truncation marker (empty when nothing was cut).
	Notice string `json:"notice,omitempty"`
	// Logs are the script's console.* lines, in order.
	Logs []string `json:"logs,omitempty"`
	// LogsTruncated reports that console output hit its own cap.
	LogsTruncated bool `json:"logs_truncated,omitempty"`
	// DurationMS is wall time spent inside the sandbox, including any
	// host.tool calls the script made.
	DurationMS int64 `json:"duration_ms"`
}

// truncationNotice is the marker appended to a capped result, naming the cap
// and the remedy.
const truncationNotice = "… [truncated: result exceeded the %d-byte output cap, %d bytes dropped. Filter or aggregate inside the script and return less.]"

// --- Program cache ----------------------------------------------------------

// programCache is a bounded LRU of compiled programs keyed by source SHA-256.
// A *goja.Program is safe to run in several runtimes at once, so the compiled
// code is shared across executions while no state is.
type programCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	max     int
	hits    uint64
	misses  uint64
}

type cacheEntry struct {
	key  string
	prog *goja.Program
}

func newProgramCache(max int) *programCache {
	if max <= 0 {
		max = programCacheEntries
	}
	return &programCache{
		entries: make(map[string]*list.Element, max),
		order:   list.New(),
		max:     max,
	}
}

// compile returns the compiled program for src, compiling on a miss. Two
// concurrent misses of the same source compile twice; the second insert wins,
// which is a wasted compile, never a wrong answer.
func (c *programCache) compile(name, src string) (*goja.Program, error) {
	sum := sha256.Sum256([]byte(src))
	key := hex.EncodeToString(sum[:])

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		c.hits++
		prog := el.Value.(*cacheEntry).prog
		c.mu.Unlock()
		return prog, nil
	}
	c.misses++
	c.mu.Unlock()

	// Strict mode, always: an implicit global is a ReferenceError, not a
	// silent write onto a globalThis that is discarded a moment later.
	prog, err := goja.Compile(name, src, true)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*cacheEntry).prog, nil
	}
	el := c.order.PushFront(&cacheEntry{key: key, prog: prog})
	c.entries[key] = el
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
	return prog, nil
}

func (c *programCache) stats() (hits, misses uint64, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.order.Len()
}

// --- Sandbox ----------------------------------------------------------------

// sandbox owns the limits, the program cache, the host seam and the
// lifecycle. Toolset (tools.go) is the engine-facing wrapper around it.
type sandbox struct {
	deadline    time.Duration
	maxDeadline time.Duration
	outputCap   int
	maxStack    int
	cache       *programCache

	// host is late-bindable: the aggregate tools repo the bridge calls is
	// built from the map this toolset is registered in. See SetHost.
	hostMu sync.RWMutex
	host   HostToolCaller

	// Lifecycle: stopOnce guards the close, wg joins in-flight work, and live
	// lets Shutdown interrupt VMs that are still running.
	mu       sync.Mutex
	live     map[*goja.Runtime]struct{}
	stopped  bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// begin admits an execution, joining it to the WaitGroup Shutdown waits on
// and registering its VM so Shutdown can interrupt it. Returns false once the
// sandbox is stopped.
func (s *sandbox) begin(vm *goja.Runtime) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.live[vm] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *sandbox) end(vm *goja.Runtime) {
	s.mu.Lock()
	delete(s.live, vm)
	s.mu.Unlock()
	s.wg.Done()
}

func newSandbox(deadline, maxDeadline time.Duration, outputCap, maxStack int) *sandbox {
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	if maxDeadline <= 0 {
		maxDeadline = MaxDeadline
	}
	if maxDeadline > MaxDeadline {
		maxDeadline = MaxDeadline
	}
	if deadline > maxDeadline {
		deadline = maxDeadline
	}
	if deadline < minDeadline {
		deadline = minDeadline
	}
	if outputCap <= 0 {
		outputCap = DefaultOutputCap
	}
	if outputCap > maxOutputCap {
		outputCap = maxOutputCap
	}
	if maxStack <= 0 {
		maxStack = defaultMaxCallStack
	}
	return &sandbox{
		deadline:    deadline,
		maxDeadline: maxDeadline,
		outputCap:   outputCap,
		maxStack:    maxStack,
		cache:       newProgramCache(programCacheEntries),
		live:        map[*goja.Runtime]struct{}{},
		stop:        make(chan struct{}),
	}
}

// clampDeadline resolves a requested per-call/per-script deadline against the
// configured default and the hard ceiling. A non-positive request takes the
// default; anything above the ceiling is clamped rather than refused.
func (s *sandbox) clampDeadline(requested time.Duration) time.Duration {
	if requested <= 0 {
		return s.deadline
	}
	if requested < minDeadline {
		return minDeadline
	}
	if requested > s.maxDeadline {
		return s.maxDeadline
	}
	return requested
}

func (s *sandbox) shuttingDown() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// shutdown refuses further executions, interrupts every live VM, and joins
// in-flight work, bounded by shutdownGrace.
func (s *sandbox) shutdown() {
	s.stopOnce.Do(func() { close(s.stop) })

	// stopped is set under the same lock that admits an execution, so an exec
	// that got in has already joined the WaitGroup before this snapshot is
	// taken — otherwise wg.Add could race wg.Wait.
	s.mu.Lock()
	s.stopped = true
	vms := make([]*goja.Runtime, 0, len(s.live))
	for vm := range s.live {
		vms = append(vms, vm)
	}
	s.mu.Unlock()
	for _, vm := range vms {
		vm.Interrupt(ErrShutdown)
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
}

// execSpec is one unit of sandboxed work.
type execSpec struct {
	// label names the caller in errors: "goja_eval", a script's tool name, or
	// "load <file>".
	label string
	// deadline is the already-clamped budget for this execution.
	deadline time.Duration
	// hostEnabled decides whether host.tool is callable. Load-time execution
	// sets it false, since a file's top level runs before the tool exists.
	hostEnabled bool
	// reach is the script's declared `tools` allowlist, or nil for
	// unrestricted. See declaredReach.
	reach *declaredReach
	// body runs inside the prepared VM and returns the value to marshal.
	body func(vm *goja.Runtime, codec jsonCodec) (goja.Value, error)
}

// run executes spec in a fresh runtime under a watchdog and returns the
// marshaled result: a new goja.Runtime per call so no state survives across
// executions, a joined watchdog that interrupts at the deadline or on context
// cancellation, recover() so a panic in a bridged host function becomes an
// error instead of taking the process down, and JSON-only marshaling with an
// output cap.
//
// goja's Interrupt only fires between VM instructions — it does not preempt
// a native built-in (a catastrophic-backtracking regexp, a large
// String.repeat), so such code can run past the deadline before this
// function's watchdog takes effect.
func (s *sandbox) run(ctx context.Context, spec execSpec) (res *Result, err error) {
	if s.shuttingDown() {
		return nil, fmt.Errorf("%w: %s cannot start %s", ErrShutdown, spec.label, severityRecoverable)
	}

	vm := goja.New()
	vm.SetMaxCallStackSize(s.maxStack)

	codec, cerr := newJSONCodec(vm)
	if cerr != nil {
		return nil, fmt.Errorf("goja: %s: runtime has no usable JSON codec: %w", spec.label, cerr)
	}
	console := &consoleBuffer{}
	if err := installConsole(vm, codec, console); err != nil {
		return nil, fmt.Errorf("goja: %s: could not install console: %w", spec.label, err)
	}
	hs := newHostState()
	hs.reach = spec.reach
	if err := s.installHost(ctx, vm, codec, spec.hostEnabled, spec.label, hs); err != nil {
		return nil, fmt.Errorf("goja: %s: could not install host bridge: %w", spec.label, err)
	}

	if !s.begin(vm) {
		return nil, fmt.Errorf("%w: %s cannot start %s", ErrShutdown, spec.label, severityRecoverable)
	}
	defer s.end(vm)

	// One timer goroutine, joined unconditionally via the deferred
	// close(fin)+Wait below, so nothing outlives this call even if body
	// panics. The clock stops while the script is inside host.tool (see
	// hostState.stopClock); the non-compute interrupts (caller context,
	// shutdown) stay live throughout so a paused deadline never makes an
	// execution unkillable.
	fin := make(chan struct{})
	var watchdog sync.WaitGroup
	watchdog.Add(1)
	go func() {
		defer watchdog.Done()
		remaining := spec.deadline
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		armed := time.Now()
		for {
			select {
			case <-fin:
				return
			case <-timer.C:
				vm.Interrupt(ErrDeadline)
				return
			case <-ctx.Done():
				vm.Interrupt(ctx.Err())
				return
			case <-s.stop:
				vm.Interrupt(ErrShutdown)
				return
			case <-hs.paused:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				if remaining -= time.Since(armed); remaining < 0 {
					remaining = 0
				}
				select {
				case <-hs.resumed:
					armed = time.Now()
					timer.Reset(remaining)
				case <-fin:
					return
				case <-ctx.Done():
					vm.Interrupt(ctx.Err())
					return
				case <-s.stop:
					vm.Interrupt(ErrShutdown)
					return
				}
			}
		}
	}()

	started := time.Now()
	defer func() {
		close(fin)
		watchdog.Wait()
		// A panic from a bridged host function propagates out of RunProgram,
		// so without this recover the whole process dies for one bad tool.
		if r := recover(); r != nil {
			res = nil
			err = recoverablef("goja: %s: the host tool path panicked mid-call: %v. This is a defect in the tool that was called, not in the script; try a different tool or different arguments", spec.label, clampText(fmt.Sprint(r), maxErrorTextBytes))
		}
	}()

	value, berr := spec.body(vm, codec)
	elapsed := time.Since(started)
	if berr != nil {
		return nil, s.execError(spec, hs, berr, elapsed)
	}

	raw, merr := codec.marshal(value)
	if merr != nil {
		return nil, s.execError(spec, hs, merr, elapsed)
	}

	out := &Result{
		Value:      raw,
		DurationMS: elapsed.Milliseconds(),
	}
	if len(raw) > s.outputCap {
		dropped := len(raw) - s.outputCap
		notice := fmt.Sprintf(truncationNotice, s.outputCap, dropped)
		head := truncateAtRune(string(raw[:s.outputCap]))
		capped, jerr := json.Marshal(head + notice)
		if jerr != nil { // unreachable: a Go string always marshals
			return nil, fmt.Errorf("goja: %s: could not encode the truncated result: %w", spec.label, jerr)
		}
		out.Value = capped
		out.Truncated = true
		out.Notice = notice
	}
	out.Logs, out.LogsTruncated = console.lines()
	return out, nil
}

// execError turns a goja failure into a teaching error, keeping the deadline,
// throw, overflow and marshal-failure cases distinct rather than collapsing
// them into one message.
func (s *sandbox) execError(spec execSpec, hs *hostState, err error, elapsed time.Duration) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		switch v := interrupted.Value().(type) {
		case error:
			switch {
			case errors.Is(v, ErrShutdown):
				return fmt.Errorf("%w: %s was interrupted by shutdown after %s %s", ErrShutdown, spec.label, elapsed.Round(time.Millisecond), severityRecoverable)
			case errors.Is(v, context.Canceled), errors.Is(v, context.DeadlineExceeded):
				return fmt.Errorf("goja: %s was interrupted after %s because the caller's context ended: %w", spec.label, elapsed.Round(time.Millisecond), v)
			}
		}
		// Compute time, not wall time: the difference is whatever the script
		// spent parked in host.tool, which the deadline no longer counts.
		compute := elapsed
		if hs != nil {
			compute -= hs.hostWait
		}
		if compute < 0 {
			compute = 0
		}
		return fmt.Errorf("goja: %s spent %s computing and was interrupted at its %s deadline; no value was produced. Do less work per call, or raise deadline_ms (ceiling %s): %w %s",
			spec.label, compute.Round(time.Millisecond), spec.deadline, s.maxDeadline, ErrDeadline, severityRecoverable)
	}

	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		return recoverablef("goja: %s exceeded the maximum call depth of %d frames. Rewrite the recursion as a loop, or bound it", spec.label, s.maxStack)
	}

	var ex *goja.Exception
	if errors.As(err, &ex) {
		thrown := recoverablef("goja: %s threw: %s", spec.label, clampText(exceptionText(ex), maxErrorTextBytes))
		// An uncaught bridge refusal keeps its sentinel so a caller branching
		// on ErrRecursionRefused need not string-match the crossed message.
		if hs != nil && hs.refusal != nil && strings.Contains(thrown.Error(), hs.refusal.Error()) {
			return &taggedError{msg: thrown.Error(), err: hs.refusal}
		}
		return thrown
	}

	if errors.Is(err, ErrNotJSON) || errors.Is(err, ErrRecursionRefused) || errors.Is(err, ErrHostUnavailable) || errors.Is(err, ErrToolUndeclared) || errors.Is(err, ErrToolNotData) {
		return err
	}
	if strings.Contains(err.Error(), severityRecoverable) || strings.Contains(err.Error(), severityFatalToken) {
		return err
	}
	return recoverablef("goja: %s: %s", spec.label, clampText(err.Error(), maxErrorTextBytes))
}

// taggedError carries a clean model-facing message and a sentinel for
// errors.Is, for an error that left Go, became a JS exception, and came back
// with the sentinel no longer in the chain.
type taggedError struct {
	msg string
	err error
}

func (e *taggedError) Error() string { return e.msg }
func (e *taggedError) Unwrap() error { return e.err }

// exceptionText renders a thrown JS value for a model-facing error, keeping
// the JS source position but dropping a stack whose top frame is native Go.
func exceptionText(ex *goja.Exception) string {
	full := strings.TrimSpace(ex.Error())
	if strings.Contains(full, "(native)") {
		if v := ex.Value(); v != nil {
			return strings.TrimSpace(v.String())
		}
	}
	return full
}

// runSource compiles (or reuses) src and runs it, returning the program's
// completion value — the goja_eval path.
func (s *sandbox) runSource(ctx context.Context, label, src string, deadline time.Duration) (*Result, error) {
	if len(src) > maxSourceBytes {
		return nil, recoverablef("goja: %s: source is %d bytes, over the %d-byte limit. Send less code — build the data with a tool call and transform it here, rather than embedding it in the source", label, len(src), maxSourceBytes)
	}
	prog, err := s.cache.compile(label, src)
	if err != nil {
		return nil, recoverablef("goja: %s did not parse: %s", label, clampText(err.Error(), maxErrorTextBytes))
	}
	return s.run(ctx, execSpec{
		label:       label,
		deadline:    deadline,
		hostEnabled: true,
		body: func(vm *goja.Runtime, _ jsonCodec) (goja.Value, error) {
			return vm.RunProgram(prog)
		},
	})
}

// --- JSON-only marshaling ---------------------------------------------------
//
// Both directions cross through the runtime's own JSON.parse / JSON.stringify
// rather than goja's Go<->JS value mapping, because goja.ToValue on a Go map
// produces a wrapper that shares the map with the host, while JSON.parse
// produces a plain JS object that shares nothing. Cost: NaN and ±Infinity
// become null, undefined becomes null at the top level, and an unrepresentable
// value is refused with a teaching error rather than silently dropped.

type jsonCodec struct {
	vm        *goja.Runtime
	parse     goja.Callable
	stringify goja.Callable
}

func newJSONCodec(vm *goja.Runtime) (jsonCodec, error) {
	obj, ok := vm.Get("JSON").(*goja.Object)
	if !ok {
		return jsonCodec{}, errors.New("JSON global missing")
	}
	parse, ok := goja.AssertFunction(obj.Get("parse"))
	if !ok {
		return jsonCodec{}, errors.New("JSON.parse is not callable")
	}
	stringify, ok := goja.AssertFunction(obj.Get("stringify"))
	if !ok {
		return jsonCodec{}, errors.New("JSON.stringify is not callable")
	}
	return jsonCodec{vm: vm, parse: parse, stringify: stringify}, nil
}

// toJS turns Go-side JSON into a plain JS value.
func (c jsonCodec) toJS(raw []byte) (goja.Value, error) {
	if len(raw) == 0 {
		raw = []byte("null")
	}
	return c.parse(goja.Undefined(), c.vm.ToValue(string(raw)))
}

// marshal renders a JS value as JSON bytes, refusing what JSON cannot carry.
func (c jsonCodec) marshal(v goja.Value) ([]byte, error) {
	if v == nil || goja.IsUndefined(v) {
		// A script that returns nothing returns null, not an error.
		return []byte("null"), nil
	}
	if _, ok := goja.AssertFunction(v); ok {
		return nil, wrapRecoverable(ErrNotJSON, "returned a function. Return data — call the function and return its result")
	}
	if _, ok := v.(*goja.Symbol); ok {
		return nil, wrapRecoverable(ErrNotJSON, "returned a symbol. Return a string, number, boolean, array, object or null")
	}
	if obj, ok := v.(*goja.Object); ok {
		if _, isPromise := obj.Export().(*goja.Promise); isPromise {
			// JSON.stringify would otherwise answer "{}" here, silently.
			return nil, wrapRecoverable(ErrNotJSON, "returned a Promise. This sandbox has no event loop in V1: write synchronous code and return the value itself. host.tool is synchronous — it returns the tool's result directly, never a promise")
		}
	}

	out, err := c.stringify(goja.Undefined(), v)
	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			return nil, err // deadline/cancel: let execError classify it
		}
		var ex *goja.Exception
		if errors.As(err, &ex) {
			// Canonical case: a cycle, or a BigInt.
			return nil, wrapRecoverable(ErrNotJSON, "could not be serialised: %s. Return a plain data structure — no cycles, no BigInt", clampText(exceptionText(ex), maxErrorTextBytes))
		}
		return nil, wrapRecoverable(ErrNotJSON, "could not be serialised: %s", clampText(err.Error(), maxErrorTextBytes))
	}
	if out == nil || goja.IsUndefined(out) {
		// JSON.stringify answers `undefined` for values it has no encoding for.
		return nil, wrapRecoverable(ErrNotJSON, "returned a value JSON cannot represent (a function, a symbol, or undefined nested where a value was required)")
	}
	text := out.String()
	if !json.Valid([]byte(text)) { // unreachable in practice; a corrupt exit is worse than a refusal
		return nil, wrapRecoverable(ErrNotJSON, "produced invalid JSON")
	}
	return []byte(text), nil
}

// --- console ----------------------------------------------------------------
// console.log is not ambient I/O: it appends to a bounded buffer that travels
// back in the Result.

type consoleBuffer struct {
	mu        sync.Mutex
	buf       []string
	bytes     int
	truncated bool
}

func (c *consoleBuffer) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(line) > maxLogLineBytes {
		line = truncateAtRune(line[:maxLogLineBytes]) + "…"
	}
	if len(c.buf) >= maxLogLines || c.bytes+len(line) > maxLogBytes {
		c.truncated = true
		return
	}
	c.buf = append(c.buf, line)
	c.bytes += len(line)
}

func (c *consoleBuffer) lines() ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 {
		return nil, c.truncated
	}
	out := make([]string, len(c.buf))
	copy(out, c.buf)
	return out, c.truncated
}

func installConsole(vm *goja.Runtime, codec jsonCodec, buf *consoleBuffer) error {
	obj := vm.NewObject()
	write := func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, arg := range call.Arguments {
				parts = append(parts, consoleFormat(codec, arg))
			}
			buf.add(level + strings.Join(parts, " "))
			return goja.Undefined()
		}
	}
	for name, level := range map[string]string{
		"log":   "",
		"info":  "",
		"debug": "",
		"warn":  "warn: ",
		"error": "error: ",
	} {
		if err := obj.Set(name, write(level)); err != nil {
			return err
		}
	}
	return vm.Set("console", obj)
}

// consoleFormat renders one console argument: strings verbatim, everything
// else as JSON where possible, falling back to the value's own string form.
func consoleFormat(codec jsonCodec, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	if _, ok := v.(*goja.Symbol); ok {
		return v.String()
	}
	if s, ok := v.Export().(string); ok {
		return s
	}
	if out, err := codec.stringify(goja.Undefined(), v); err == nil && out != nil && !goja.IsUndefined(out) {
		return out.String()
	}
	return v.String()
}
