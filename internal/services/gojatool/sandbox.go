// Package gojatool is the agent's embedded ECMAScript sandbox: ONE bounded goja
// runtime behind two capabilities —
//
//   - SCRIPT TOOLS: operator-authored `*.js` files loaded from a configured
//     directory and registered as ordinary tools (the lowest-friction
//     extensibility this runtime can offer: no Go toolchain, no MCP server), and
//   - goja_eval: a sandbox tool the model drives directly (the code-interpreter
//     pattern) for bounded compute — transforms, parsing, arithmetic over tool
//     results.
//
// See docs/development/blueprints/goja-tools.md for the ratified design. Five
// decisions shape every file here:
//
//   - NAMED goja, NEVER js. "js" drags browser/Node priors into the model's tool
//     choice; `goja` is a distinctive token that means exactly this sandbox. The
//     provider is the namespace, so script tools are registered under their
//     DECLARED name, unprefixed (see tools.go).
//
//   - THE ONE BOUNDARY RULE. Scripts have no ambient I/O: no filesystem, no
//     network, no require, no process. Their only reach into the world is
//     host.tool(name, args), which routes through the SAME tool execution path
//     the model uses — HITL wrapper included. A script calling
//     local_fs.write_file meets the same envelope a model call would. One policy
//     boundary, unchanged. Scripts may not invoke goja-provider tools: depth is
//     exactly one (see bridge.go).
//
//   - A TOOL RESULT IS NOT A CONTRACT. Tool answers are written for a READER,
//     and a script that parses one is guessing at a format nobody promised.
//     host.tool therefore never hands a script prose as a bare string: text
//     arrives as {text: "…"} whose string operations throw a teaching error,
//     structured results arrive as data, and a stand-in answer (a cache stub, a
//     refusal) is redeemed or thrown rather than passed off as content. Live use
//     found this too: a script read "4 staged, 2 other, no untracked" off a tree
//     with one modified and one untracked file, and returned successfully. See
//     hostresult.go for the whole argument, including what a script declares
//     when it means to parse prose anyway.
//
//   - THE DEADLINE BOUNDS COMPUTE, NOT WAITING. The clock stops while a script
//     is inside host.tool, because the thing on the other side of that call may
//     be an approval card with a human in front of it, and no human answers in
//     two seconds. What bounds the reach into the world instead is maxHostCalls,
//     a separate limit on how many times one execution may make the trip. (Live
//     use found this: the original design ran the clock straight through, which
//     killed every script that touched an approve-tier tool the moment the
//     operator clicked allow.) See hostState.stopClock.
//
//   - MEMORY IS DEADLINE-BOUNDED, NOT CAPPED. This is the honest limit and it is
//     documented rather than hidden. goja has NO memory cap: the 2026-07-27
//     spike measured an allocation bomb taking 64 MB in 300 ms before the
//     interrupt stopped it, so the default 2s deadline implies a TRANSIENT
//     ceiling in the low hundreds of MB, then GC. Everything else here (the
//     interrupt watchdog, the call-stack cap, the output cap) is a hard bound;
//     this one is a rate limit. If that ceiling ever matters, the named
//     escalation is a subprocess+rlimit tier, not a knob in this package.
//
// # V1 exclusions, and why
//
//   - NO EVENT LOOP, no setTimeout/setInterval, no promise job pump. Execution
//     is synchronous: a program runs, returns a value, and the runtime is
//     thrown away. An event loop would mean a VM that outlives the call, which
//     means state that outlives the call — and per-execution isolation is the
//     property the whole safety story rests on. `async` syntax still PARSES, so
//     returning a Promise is REFUSED with a teaching error rather than marshaled
//     as `{}` — which is exactly what JSON.stringify would otherwise do to it,
//     and a silent wrong answer is the one outcome worse than a refusal.
//   - NO TypeScript. Transpilation needs esbuild-class weight; bring your own
//     build until dogfooding demands otherwise.
//   - NO module system. One file, one tool. `require`/`import` are absent, not
//     stubbed, so the failure is a plain ReferenceError at the line that tried.
//
// # The limit the interrupt cannot enforce
//
// goja's Interrupt only fires between VM instructions: it does NOT preempt a
// native built-in. A catastrophic-backtracking RegExp (goja falls back to
// regexp2, which is compiled without a match timeout) or one enormous
// String.repeat can therefore run past the deadline inside native code, and this
// package will block until it returns. Sources are size-capped and the call is
// synchronous by design — abandoning the goroutine would trade a bounded hang
// for an unbounded leak. Same escalation as memory: the subprocess tier.
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
//
// Every limit is a documented constant, and every one of them is asserted by a
// test in sandbox_test.go. The exported three are the ones an operator or a
// script author has to reason about; the rest are implementation ceilings that
// exist so no single call can consume the process.

const (
	// DefaultDeadline bounds a single execution. The blueprint's number. It is
	// also, per the package doc, the de-facto memory bound: an allocation bomb
	// grows the heap at roughly 200 MB/s until this fires.
	DefaultDeadline = 2 * time.Second

	// MaxDeadline is the ceiling on any per-call (goja_eval's deadline_ms) or
	// per-script (tool.deadline_ms) override. A script that needs more than 30s
	// of pure compute is not a tool; it is a job, and it belongs behind a
	// different door.
	MaxDeadline = 30 * time.Second

	// DefaultOutputCap bounds the marshaled JSON a single execution returns.
	// 64 KiB is already ~16k tokens of model context; past that the script
	// should be filtering, and the truncation notice says so. It bounds what is
	// RETURNED, not what the script allocates while producing it — that remains
	// the deadline's job, per the memory note above.
	DefaultOutputCap = 64 << 10
)

const (
	// minDeadline floors any override. Below this the watchdog fires before the
	// VM has finished warming up, which reads to the model as a broken tool
	// rather than a too-small budget.
	minDeadline = 10 * time.Millisecond

	// maxOutputCap ceilings a configured OutputCap. An operator raising the cap
	// is raising the model's per-call context spend; 1 MiB is the point past
	// which that is certainly a mistake.
	maxOutputCap = 1 << 20

	// defaultMaxCallStack is the goja SetMaxCallStackSize value. goja's own
	// default is math.MaxInt32 — i.e. unbounded, so `function f(){return f()}`
	// grows a Go slice until the process dies. 512 frames is far more than any
	// legitimate recursive JSON walk needs and is contained in microseconds.
	defaultMaxCallStack = 512

	// maxSourceBytes bounds one script file and one goja_eval `code` argument.
	// The cap exists because compilation itself is unbounded work that the
	// interrupt cannot preempt (the compiler is native Go, not VM instructions).
	maxSourceBytes = 128 << 10

	// programCacheEntries bounds the compiled-program LRU, keyed by source
	// SHA-256. Worst case retained: this many programs of maxSourceBytes each.
	programCacheEntries = 64

	// Console capture bounds. console.* writes to a buffer returned with the
	// result — it is not ambient I/O — but a model that logs in a loop must not
	// be able to spend the context window through the back door.
	maxLogLines     = 100
	maxLogLineBytes = 1 << 10
	maxLogBytes     = 8 << 10

	// maxHostCalls bounds how many times ONE execution may reach the world
	// through host.tool.
	//
	// It exists because the deadline stops while a host call is in flight (see
	// hostState.stopClock: the thing on the other side may be a human reading an
	// approval card), and a paused clock cannot bound `while(true){
	// host.tool(...) }` — each iteration is nearly free in COMPUTE terms. So the
	// reach into the world gets its own limit, orthogonal to time, in the same
	// spirit as the output and stack caps.
	//
	// 256 is far past what a tool does and far short of what a job does: the
	// example scripts make one or two calls, a script fanning out over a
	// directory makes dozens, and a script that wants more than this is not a
	// tool call — it is a pipeline, and it belongs behind a chain.
	maxHostCalls = 256

	// maxErrorTextBytes clamps a model-facing error built from script-controlled
	// text (a thrown message, a JS stack). Without it, `throw "x".repeat(1e7)` is
	// an output channel that ignores the output cap.
	maxErrorTextBytes = 2 << 10

	// maxEchoRunes bounds how much of a MODEL-SUPPLIED string a teaching error
	// quotes back — same rule and same number as gointel (loader.go): every
	// argument on this surface is model-written, so an error that echoes one
	// verbatim is a channel whose length the model controls.
	maxEchoRunes = 120

	// shutdownGrace bounds how long Shutdown waits for in-flight executions.
	// It is bounded rather than unbounded because an in-flight host.tool call
	// may be parked on a HUMAN approval, and process teardown must not hang on a
	// human. Script time itself is already bounded by MaxDeadline.
	shutdownGrace = 5 * time.Second
)

// --- Errors -----------------------------------------------------------------
//
// Voice follows local_fs (internal/services/localtools/fs.go) and gointel: a
// "goja: " prefix, the concrete value that failed, and the next call that would
// work. The severity marker is localtools' fatal-vs-recoverable convention
// (internal/services/localtools/hardening.go). Everything this package refuses
// is recoverable by a corrected call — a smaller result, a fixed script, a
// different tool — so nothing here is marked fatal.

const (
	severityRecoverable = "(recoverable: adjust parameters and retry)"
	severityFatalToken  = "(fatal:"
)

// Sentinel errors, for errors.Is by callers and tests.
var (
	// ErrDeadline means the execution was interrupted at its deadline. The
	// script did not finish and produced no value.
	ErrDeadline = errors.New("goja: deadline exceeded")
	// ErrShutdown means the toolset has been shut down and will run nothing
	// further. It is the typed answer to a call that raced engine teardown.
	ErrShutdown = errors.New("goja: sandbox shut down")
	// ErrHostUnavailable means host.tool was called but no HostToolCaller is
	// wired. Scripts that only compute never see it.
	ErrHostUnavailable = errors.New("goja: host tool path unavailable")
	// ErrRecursionRefused means a script tried to invoke a goja-provider tool.
	// Depth is exactly one, by design.
	ErrRecursionRefused = errors.New("goja: recursive goja tool call refused")
	// ErrToolDenied means a host.tool call was refused by the approval envelope.
	// It is a REFUSAL, not a result — see IsDenyMessage for why the distinction
	// has to be made here rather than left to the script.
	ErrToolDenied = errors.New("goja: host tool call denied")
	// ErrToolUndeclared means a script called a tool its descriptor does not
	// list in `tools: [...]`. The declaration is optional; once made, it is
	// enforced. See declaredReach.
	ErrToolUndeclared = errors.New("goja: host tool not declared by this script")
	// ErrToolNotData means a tool answered with a stand-in that has no meaning
	// for a program — a stub, a notice, a refusal. It is thrown rather than
	// handed over, because a sentence that looks like content is the one thing
	// a script cannot detect. See hostresult.go.
	ErrToolNotData = errors.New("goja: host tool result is not data")
	// ErrHostBudget means one execution exhausted its host.tool budget
	// (maxHostCalls). It is the bound on reaching the world that the deadline
	// stopped being, once the clock learned to stop for a human.
	ErrHostBudget = errors.New("goja: host tool budget exhausted")
	// ErrScriptLoad means a script file failed fail-fast validation at load.
	// Every wrapped message names the file.
	ErrScriptLoad = errors.New("goja: script load failed")
	// ErrNotJSON means a value could not cross the JSON-only boundary.
	ErrNotJSON = errors.New("goja: value is not JSON-representable")
)

// recoverablef builds a teaching error tagged recoverable-by-correction.
func recoverablef(format string, a ...any) error {
	return errors.New(fmt.Sprintf(format, a...) + " " + severityRecoverable)
}

// wrapRecoverable tags a sentinel-wrapping error recoverable unless it already
// carries a severity marker (gointel's wrapRecoverable, same shape).
func wrapRecoverable(sentinel error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, severityRecoverable) || strings.Contains(msg, severityFatalToken) {
		return fmt.Errorf("%w: %s", sentinel, msg)
	}
	return fmt.Errorf("%w: %s %s", sentinel, msg, severityRecoverable)
}

// markSeverity tags err recoverable-by-correction unless it is already tagged.
// The wrap preserves the error chain (errors.Is still works) while appending the
// marker to the rendered text. It is applied at the Exec boundary exactly as
// LocalFSTools.Exec applies it, so the convention holds on every return path
// rather than on the paths someone remembered.
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

// withoutSeverity strips a severity marker from text that is about to be
// EMBEDDED in another error, so a nested message does not end up carrying two
// markers — a model grepping for the convention should find exactly one.
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
// Go-quoted so control characters, NULs and bidi overrides are escaped rather
// than embedded in the result (gointel's echoArg).
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

// Result is what every goja tool returns to the engine (DataTypeJSON). It is a
// struct rather than a bare value so the two facts a model needs in order to
// react — was this truncated, and what did the script log — travel WITH the
// value instead of being inferred from its shape.
type Result struct {
	// Value is the script's return value, marshaled as JSON. On truncation it
	// becomes a JSON string holding the head of the original plus Notice, so the
	// envelope this sits in is always valid JSON.
	Value json.RawMessage `json:"value"`
	// Truncated reports that Value hit the output cap.
	Truncated bool `json:"truncated,omitempty"`
	// Notice is the explicit truncation marker (empty when nothing was cut).
	Notice string `json:"notice,omitempty"`
	// Logs are the script's console.* lines, in order.
	Logs []string `json:"logs,omitempty"`
	// LogsTruncated reports that console output hit its own cap.
	LogsTruncated bool `json:"logs_truncated,omitempty"`
	// DurationMS is wall time spent inside the sandbox, including any host.tool
	// calls the script made.
	DurationMS int64 `json:"duration_ms"`
}

// truncationNotice is the explicit marker appended to a capped result. It names
// the cap and the remedy, because "output was truncated" without a next action
// is a turn the model spends guessing.
const truncationNotice = "… [truncated: result exceeded the %d-byte output cap, %d bytes dropped. Filter or aggregate inside the script and return less.]"

// --- Program cache ----------------------------------------------------------

// programCache is a bounded LRU of compiled programs keyed by source SHA-256.
// A *goja.Program is runtime-independent and safe to run in several runtimes at
// once, which is what makes the cache compatible with fresh-VM-per-execution:
// the COMPILED CODE is shared, no state is.
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
// concurrent misses of the same source compile twice and the second insert wins;
// that is a wasted compile, never a wrong answer, and it is cheaper than holding
// the lock across compilation.
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

	// Strict mode, always: an accidental implicit global (`x = 5`) is a
	// ReferenceError the author sees immediately rather than a silent write onto
	// a globalThis that is discarded a millisecond later.
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

// sandbox owns the limits, the program cache, the host seam and the lifecycle.
// It is the whole safety story; Toolset (tools.go) is the engine-facing wrapper
// around it.
type sandbox struct {
	deadline    time.Duration
	maxDeadline time.Duration
	outputCap   int
	maxStack    int
	cache       *programCache

	// host is late-bindable because of an unavoidable construction cycle: the
	// aggregate tools repo the bridge calls is built FROM the map this toolset is
	// registered in. See SetHost.
	hostMu sync.RWMutex
	host   HostToolCaller

	// Lifecycle, mirroring shellsession.manager: stopOnce guards the close, wg
	// joins in-flight work, and live lets Shutdown interrupt VMs that are still
	// running instead of waiting out their deadlines.
	mu       sync.Mutex
	live     map[*goja.Runtime]struct{}
	stopped  bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// begin admits an execution, joining it to the WaitGroup Shutdown waits on and
// registering its VM so Shutdown can interrupt it. Returns false once the
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
// default; anything above the ceiling is clamped rather than refused, because
// refusing costs the model a turn to learn a number the schema already states.
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
// in-flight work — bounded by shutdownGrace (see the constant for why bounded).
func (s *sandbox) shutdown() {
	s.stopOnce.Do(func() { close(s.stop) })

	// stopped is set under the SAME lock that admits an execution, so an exec
	// that got in has already joined the WaitGroup before this snapshot is taken.
	// Without that ordering, wg.Add could race wg.Wait.
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
	// sets it false: a file's top level runs before the tool exists, so a call
	// out to the world from there is a mistake worth naming.
	hostEnabled bool
	// reach is the script's declared `tools` allowlist, or nil for unrestricted
	// (goja_eval, and scripts that declare none). See declaredReach.
	reach *declaredReach
	// body runs inside the prepared VM and returns the value to marshal.
	body func(vm *goja.Runtime, codec jsonCodec) (goja.Value, error)
}

// run executes spec in a FRESH runtime under a watchdog and returns the
// marshaled result. Everything that makes this safe lives here:
//
//   - a new goja.Runtime per execution (no state survives a call, so no script
//     can observe or corrupt another — asserted by the isolation test),
//   - SetMaxCallStackSize, so recursion is contained rather than fatal,
//   - a joined watchdog goroutine that interrupts at the deadline or on context
//     cancellation (never leaked: the deferred close/Wait pair runs on every
//     path, including panic),
//   - recover() at this boundary, so a Go panic raised inside a bridged host
//     function becomes an error instead of taking the process down,
//   - JSON-only marshaling both directions, and
//   - the output cap with an explicit truncation marker.
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

	// The watchdog. One timer goroutine, joined unconditionally: the deferred
	// close(fin)+Wait below runs even when body panics, so no goroutine and no
	// timer outlives this call. Interrupt is safe to call from another goroutine
	// (goja guards it with an atomic) and the flag is sticky, so an interrupt
	// raised while a native call is in flight takes effect the moment that call
	// returns.
	//
	// The clock STOPS while the script is inside host.tool — see
	// hostState.stopClock for why, and for what still bounds that wait. The
	// three interrupts that are not about compute (caller context, shutdown)
	// remain live throughout: pausing the deadline must never make an execution
	// unkillable.
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
		// A panic from a bridged host function propagates out of RunProgram
		// (goja re-panics anything that is not one of its own exception types),
		// so without this the whole process dies because one tool had a bug.
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

// execError turns a goja failure into a teaching error. The four cases are
// genuinely different for the model — a deadline means "do less work", a throw
// means "fix this line", an overflow means "stop recursing", a marshal failure
// means "return data, not objects" — so they are never collapsed into one
// message.
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
		// COMPUTE time, not wall time: the difference is whatever the script spent
		// parked in host.tool, which the deadline no longer counts (stopClock).
		// Reporting the wall figure here would tell a model to "do less work" when
		// what actually took four minutes was an operator reading an approval card.
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
		// An UNCAUGHT bridge refusal keeps its sentinel: the guard's decision is
		// what ended the run, and a caller branching on ErrRecursionRefused should
		// not have to string-match a message that crossed the JS boundary.
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
// errors.Is. It exists for exactly one case: an error that left Go, became a JS
// exception, and came back — where the sentinel is known but no longer in the
// chain.
type taggedError struct {
	msg string
	err error
}

func (e *taggedError) Error() string { return e.msg }
func (e *taggedError) Unwrap() error { return e.err }

// exceptionText renders a thrown JS value for a model-facing error. The JS
// source position is kept — "at goja_eval:3:11" is the single most useful thing
// in the message when the model is fixing its own code — but a stack whose top
// frame is native Go is dropped: a Go symbol path teaches the model nothing and
// leaks this package's internals into the transcript.
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
// rather than goja's Go<->JS value mapping, and that is a deliberate boundary
// rather than convenience:
//
//   - goja.ToValue on a Go map produces a WRAPPER that shares the Go map with
//     the host, so a script mutating its own arguments would reach back into
//     engine memory. JSON.parse produces a plain JS object that shares nothing.
//   - JSON.stringify is the one exit that provably cannot carry a live
//     reference — no functions, no symbols, no Go pointers, no cycles.
//
// The cost is honest and documented: NaN and ±Infinity become null (JSON has no
// spelling for them), undefined becomes null at the top level, and a value that
// cannot be represented is refused with a teaching error rather than silently
// dropped.

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
		// A script that returns nothing returns null, not an error: "no value"
		// is a legitimate answer and JSON's spelling for it is null.
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
			// JSON.stringify would answer "{}" here — a silent wrong answer for
			// the one V1 exclusion a model is most likely to trip over.
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
			// The canonical case is a cycle ("Converting circular structure to
			// JSON"); BigInt lands here too. The engine's own message is the
			// clearest possible statement of what went wrong, so it is kept.
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
//
// console.log is NOT ambient I/O: it appends to a bounded buffer that travels
// back in the Result. It exists because a model writing JS writes console.log by
// reflex, and a ReferenceError at that line costs a whole turn to learn a rule
// that teaches the model nothing about its actual task.

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

// consoleFormat renders one console argument: strings verbatim, everything else
// as JSON where possible, falling back to the value's own string form (a cyclic
// object logs as [object Object] rather than failing the whole call).
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
