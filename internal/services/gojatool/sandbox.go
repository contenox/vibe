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

const (
	// DefaultDeadline bounds a single execution; since goja has no memory cap, this is also the de-facto memory bound (an allocation bomb grows ~150-200 MB/s until interrupted).
	DefaultDeadline = 2 * time.Second

	// MaxDeadline is the ceiling on any per-call or per-script deadline
	// override.
	MaxDeadline = 30 * time.Second

	// DefaultOutputCap bounds the marshaled JSON a single execution returns,
	// not what the script allocates while producing it.
	DefaultOutputCap = 64 << 10

	// DefaultMaxHostCalls bounds the host.tool calls one execution may make.
	DefaultMaxHostCalls = 256
)

const (
	minDeadline = 10 * time.Millisecond

	maxOutputCap = 1 << 20

	defaultMaxCallStack = 512

	maxSourceBytes = 128 << 10

	programCacheEntries = 64

	maxLogLines     = 100
	maxLogLineBytes = 1 << 10
	maxLogBytes     = 8 << 10

	maxHostCallsCeiling = 4096

	maxErrorTextBytes = 2 << 10

	maxEchoRunes = 120

	shutdownGrace = 5 * time.Second
)

// limits are the per-execution bounds, resolved once per call so a chain-level
// policy can tighten them without rebuilding the toolset (see policy.go).
type limits struct {
	deadline     time.Duration
	maxDeadline  time.Duration
	outputCap    int
	maxHostCalls int
}

func clampLimits(l limits) limits {
	if l.maxDeadline <= 0 {
		l.maxDeadline = MaxDeadline
	}
	if l.maxDeadline > MaxDeadline {
		l.maxDeadline = MaxDeadline
	}
	if l.deadline <= 0 {
		l.deadline = DefaultDeadline
	}
	if l.deadline > l.maxDeadline {
		l.deadline = l.maxDeadline
	}
	if l.deadline < minDeadline {
		l.deadline = minDeadline
	}
	if l.outputCap <= 0 {
		l.outputCap = DefaultOutputCap
	}
	if l.outputCap > maxOutputCap {
		l.outputCap = maxOutputCap
	}
	if l.maxHostCalls <= 0 {
		l.maxHostCalls = DefaultMaxHostCalls
	}
	if l.maxHostCalls > maxHostCallsCeiling {
		l.maxHostCalls = maxHostCallsCeiling
	}
	return l
}

func (l limits) clampDeadline(requested time.Duration) time.Duration {
	if requested <= 0 {
		return l.deadline
	}
	if requested < minDeadline {
		return minDeadline
	}
	if requested > l.maxDeadline {
		return l.maxDeadline
	}
	return requested
}

// --- Errors -----------------------------------------------------------------

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
	// ErrToolDenied means a host.tool call was refused by the approval envelope (see IsDenyMessage).
	ErrToolDenied = errors.New("goja: host tool call denied")
	// ErrToolUndeclared means a script called a tool its descriptor does not
	// list in `tools: [...]`.
	ErrToolUndeclared = errors.New("goja: host tool not declared by this script")
	// ErrToolNotData means a tool answered with a stand-in that has no
	// meaning for a program — a stub, a notice, a refusal.
	ErrToolNotData = errors.New("goja: host tool result is not data")
	// ErrHostBudget means one execution exhausted its host.tool budget.
	ErrHostBudget = errors.New("goja: host tool budget exhausted")
	// ErrScriptLoad means a script file failed fail-fast validation at load.
	ErrScriptLoad = errors.New("goja: script load failed")
	// ErrNotJSON means a value could not cross the JSON-only boundary.
	ErrNotJSON = errors.New("goja: value is not JSON-representable")
)

func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

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

func withoutSeverity(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, severityRecoverable, ""))
}

func clampText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := truncateAtRune(s[:max])
	return cut + fmt.Sprintf("… (+%d more bytes)", len(s)-len(cut))
}

func echoArg(s string) string {
	r := []rune(s)
	if len(r) > maxEchoRunes {
		return fmt.Sprintf("%q… (+%d more characters)", string(r[:maxEchoRunes]), len(r)-maxEchoRunes)
	}
	return fmt.Sprintf("%q", s)
}

func truncateAtRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// --- Result -----------------------------------------------------------------

// Result is what every goja tool returns to the engine (DataTypeJSON).
type Result struct {
	// Value is the script's return value marshaled as JSON; on truncation it becomes a JSON string holding the head of the original plus Notice.
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

const truncationNotice = "… [truncated: result exceeded the %d-byte output cap, %d bytes dropped. Filter or aggregate inside the script and return less.]"

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

	// Strict mode always: an implicit global is a ReferenceError, not a silent write onto a discarded globalThis.
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

type sandbox struct {
	// provider is the name this toolset is registered under, which is both the
	// key a chain-level policy is stored against and the address a script may
	// not call back into.
	provider string
	base     limits
	maxStack int
	cache    *programCache

	hostMu sync.RWMutex
	host   HostToolCaller

	mu       sync.Mutex
	live     map[*goja.Runtime]struct{}
	stopped  bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

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

func newSandbox(cfg Config) *sandbox {
	maxStack := cfg.MaxCallStackSize
	if maxStack <= 0 {
		maxStack = defaultMaxCallStack
	}
	return &sandbox{
		provider: registeredName(cfg.Name),
		base: clampLimits(limits{
			deadline:     cfg.Deadline,
			maxDeadline:  cfg.MaxDeadline,
			outputCap:    cfg.OutputCap,
			maxHostCalls: cfg.MaxHostCalls,
		}),
		maxStack: maxStack,
		cache:    newProgramCache(programCacheEntries),
		live:     map[*goja.Runtime]struct{}{},
		stop:     make(chan struct{}),
	}
}

func (s *sandbox) shuttingDown() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *sandbox) shutdown() {
	s.stopOnce.Do(func() { close(s.stop) })

	// stopped is set under the same lock that admits an exec, so wg.Add can never race wg.Wait here.
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

type execSpec struct {
	label       string
	lim         limits
	deadline    time.Duration
	hostEnabled bool
	reach       *declaredReach
	body        func(vm *goja.Runtime, codec jsonCodec) (goja.Value, error)
}

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
	hs := newHostState(spec.lim.maxHostCalls)
	hs.reach = spec.reach
	if err := s.installHost(ctx, vm, codec, spec.hostEnabled, spec.label, hs); err != nil {
		return nil, fmt.Errorf("goja: %s: could not install host bridge: %w", spec.label, err)
	}

	if !s.begin(vm) {
		return nil, fmt.Errorf("%w: %s cannot start %s", ErrShutdown, spec.label, severityRecoverable)
	}
	defer s.end(vm)

	// fin is always closed via defer, joining this goroutine even on panic; shutdown/cancel interrupts stay live through a paused deadline so execution is never unkillable.
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
		// Without this recover, a panic from a bridged host function crashes the whole process for one bad tool.
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
	if len(raw) > spec.lim.outputCap {
		dropped := len(raw) - spec.lim.outputCap
		notice := fmt.Sprintf(truncationNotice, spec.lim.outputCap, dropped)
		head := truncateAtRune(string(raw[:spec.lim.outputCap]))
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
		// Compute time, not wall time: the difference is time spent parked in host.tool, uncounted by the deadline.
		compute := elapsed
		if hs != nil {
			compute -= hs.hostWait
		}
		if compute < 0 {
			compute = 0
		}
		return fmt.Errorf("goja: %s spent %s computing and was interrupted at its %s deadline; no value was produced. Do less work per call, or raise deadline_ms (ceiling %s): %w %s",
			spec.label, compute.Round(time.Millisecond), spec.deadline, spec.lim.maxDeadline, ErrDeadline, severityRecoverable)
	}

	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		return recoverablef("goja: %s exceeded the maximum call depth of %d frames. Rewrite the recursion as a loop, or bound it", spec.label, s.maxStack)
	}

	var ex *goja.Exception
	if errors.As(err, &ex) {
		thrown := recoverablef("goja: %s threw: %s", spec.label, clampText(exceptionText(ex), maxErrorTextBytes))
		// An uncaught bridge refusal keeps its sentinel so callers can errors.Is without string-matching.
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

type taggedError struct {
	msg string
	err error
}

func (e *taggedError) Error() string { return e.msg }
func (e *taggedError) Unwrap() error { return e.err }

func exceptionText(ex *goja.Exception) string {
	full := strings.TrimSpace(ex.Error())
	if strings.Contains(full, "(native)") {
		if v := ex.Value(); v != nil {
			return strings.TrimSpace(v.String())
		}
	}
	return full
}

func (s *sandbox) runSource(ctx context.Context, label, src string, lim limits, deadline time.Duration) (*Result, error) {
	if len(src) > maxSourceBytes {
		return nil, recoverablef("goja: %s: source is %d bytes, over the %d-byte limit. Send less code — build the data with a tool call and transform it here, rather than embedding it in the source", label, len(src), maxSourceBytes)
	}
	prog, err := s.cache.compile(label, src)
	if err != nil {
		return nil, recoverablef("goja: %s did not parse: %s", label, clampText(err.Error(), maxErrorTextBytes))
	}
	return s.run(ctx, execSpec{
		label:       label,
		lim:         lim,
		deadline:    deadline,
		hostEnabled: true,
		body: func(vm *goja.Runtime, _ jsonCodec) (goja.Value, error) {
			return vm.RunProgram(prog)
		},
	})
}

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

func (c jsonCodec) toJS(raw []byte) (goja.Value, error) {
	if len(raw) == 0 {
		raw = []byte("null")
	}
	return c.parse(goja.Undefined(), c.vm.ToValue(string(raw)))
}

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
