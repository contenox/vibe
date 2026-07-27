package gojatool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The adversarial set. Everything here is a hostile input that a model or an
// operator can supply, and the assertion in each case is the same: the process
// survives, the caller gets a typed error, and the error says what to do next.

func newTestSandbox(t *testing.T) *sandbox {
	t.Helper()
	ts, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(ts.Shutdown)
	return ts.sb
}

func eval(t *testing.T, sb *sandbox, code string, deadline time.Duration) (*Result, error) {
	t.Helper()
	return sb.runSource(context.Background(), ToolEval, code, sb.clampDeadline(deadline))
}

func mustEval(t *testing.T, sb *sandbox, code string) *Result {
	t.Helper()
	res, err := eval(t, sb, code, 0)
	if err != nil {
		t.Fatalf("eval %q: %v", code, err)
	}
	return res
}

// --- The limits themselves --------------------------------------------------

// The blueprint fixes these numbers and the seeded HITL policy is written
// against them. Pinning them here means a change cannot land silently.
func TestUnit_Sandbox_DocumentedLimits(t *testing.T) {
	if DefaultDeadline != 2*time.Second {
		t.Errorf("DefaultDeadline = %s, want 2s (blueprint)", DefaultDeadline)
	}
	if MaxDeadline != 30*time.Second {
		t.Errorf("MaxDeadline = %s, want 30s (blueprint)", MaxDeadline)
	}
	if DefaultOutputCap != 64<<10 {
		t.Errorf("DefaultOutputCap = %d, want 65536 (blueprint)", DefaultOutputCap)
	}

	sb := newTestSandbox(t)
	if sb.deadline != DefaultDeadline || sb.outputCap != DefaultOutputCap || sb.maxStack != defaultMaxCallStack {
		t.Fatalf("Config{} defaults = %s/%d/%d", sb.deadline, sb.outputCap, sb.maxStack)
	}

	// Overrides are clamped, never trusted: the ceiling belongs to the sandbox.
	if got := sb.clampDeadline(time.Hour); got != MaxDeadline {
		t.Errorf("clampDeadline(1h) = %s, want the %s ceiling", got, MaxDeadline)
	}
	if got := sb.clampDeadline(time.Nanosecond); got != minDeadline {
		t.Errorf("clampDeadline(1ns) = %s, want the %s floor", got, minDeadline)
	}
	if got := sb.clampDeadline(0); got != DefaultDeadline {
		t.Errorf("clampDeadline(0) = %s, want the default", got)
	}

	// A configured ceiling above the hard maximum is refused by construction.
	over := newSandbox(time.Hour, 5*time.Hour, 1<<30, 0)
	if over.maxDeadline != MaxDeadline || over.deadline > MaxDeadline || over.outputCap != maxOutputCap {
		t.Fatalf("out-of-range config not clamped: %s/%s/%d", over.deadline, over.maxDeadline, over.outputCap)
	}
}

// --- Bomb 1: the infinite loop ----------------------------------------------

func TestUnit_Sandbox_InfiniteLoopKilledAtDeadline(t *testing.T) {
	sb := newTestSandbox(t)

	const deadline = 150 * time.Millisecond
	start := time.Now()
	_, err := eval(t, sb, `while(true){}`, deadline)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an infinite loop returned successfully")
	}
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline", err)
	}
	// The timing bound is the point: the interrupt fires AT the deadline, not
	// eventually. Generous upper slack so a loaded CI box does not flake, tight
	// enough that a broken watchdog (never firing) fails.
	if elapsed < deadline {
		t.Errorf("killed after %s, before its own %s deadline", elapsed, deadline)
	}
	if elapsed > deadline+2*time.Second {
		t.Errorf("took %s to kill a %s execution", elapsed, deadline)
	}
	if !strings.Contains(err.Error(), "deadline") || !strings.Contains(err.Error(), severityRecoverable) {
		t.Errorf("error does not teach: %q", err)
	}
}

// --- Bomb 2: the allocation bomb --------------------------------------------

// The honest one. goja has NO memory cap, so this asserts only what is true:
// the execution RETURNS, at its deadline, and the heap it grabbed on the way is
// transient.
//
// MEASURED (2026-07-27, this machine, 150ms deadline, three runs): the bomb
// below grew HeapAlloc by 22, 29 and 29 MB before the interrupt landed — a rate
// of roughly 150-200 MB/s, matching the blueprint's spike number of 64 MB in
// 300ms. At the 2s default deadline that implies a transient ceiling in the low
// hundreds of MB, then GC. Memory here is DEADLINE-BOUNDED, NOT CAPPED; the
// named escalation if that ever matters is a subprocess+rlimit tier.
//
// The number is logged rather than asserted on purpose: asserting a memory
// ceiling this package does not enforce would be exactly the dishonesty the
// blueprint's honesty rule forbids.
func TestUnit_Sandbox_AllocationBombIsStoppedButNotCapped(t *testing.T) {
	sb := newTestSandbox(t)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	const deadline = 150 * time.Millisecond
	start := time.Now()
	_, err := eval(t, sb, `let a=[]; while(true){ a.push(new Array(65536).fill(1)) }`, deadline)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("the allocation bomb returned successfully")
	}
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline", err)
	}
	if elapsed > deadline+5*time.Second {
		t.Fatalf("the bomb ran for %s against a %s deadline", elapsed, deadline)
	}

	grewMB := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / (1 << 20)
	t.Logf("allocation bomb: stopped after %s, transient heap growth %d MB (no hard cap exists — this is a rate limit, not a bound)", elapsed.Round(time.Millisecond), grewMB)
}

// --- Bomb 3: recursion ------------------------------------------------------

func TestUnit_Sandbox_DeepRecursionIsContained(t *testing.T) {
	sb := newTestSandbox(t)

	start := time.Now()
	_, err := eval(t, sb, `function f(){ return f() }; f()`, time.Second)
	if err == nil {
		t.Fatal("unbounded recursion returned successfully")
	}
	// Contained by the stack cap, not by the deadline: it must fail fast.
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("recursion took %s to be contained; the call-stack cap is not doing the work", time.Since(start))
	}
	if !strings.Contains(err.Error(), fmt.Sprint(defaultMaxCallStack)) {
		t.Errorf("error does not name the %d-frame cap: %q", defaultMaxCallStack, err)
	}

	// Mutual recursion is the same bomb with two names.
	if _, err := eval(t, sb, `function a(){return b()}; function b(){return a()}; a()`, time.Second); err == nil {
		t.Fatal("mutual recursion returned successfully")
	}

	// And legitimate recursion still works.
	res := mustEval(t, sb, `function d(n){ return n === 0 ? 0 : 1 + d(n-1) }; d(200)`)
	if string(res.Value) != "200" {
		t.Errorf("legitimate 200-deep recursion = %s, want 200", res.Value)
	}
}

// --- The JSON-only boundary -------------------------------------------------

func TestUnit_Sandbox_MarshalingEdgeCases(t *testing.T) {
	sb := newTestSandbox(t)

	for _, tc := range []struct {
		name string
		code string
		want string
	}{
		{"undefined becomes null", `undefined`, `null`},
		{"a statement program returns null", `let x = 1;`, `null`},
		{"null stays null", `null`, `null`},
		// JSON has no spelling for these. The loss is documented, not hidden.
		{"NaN and Infinity become null", `({n: NaN, p: Infinity, m: -Infinity})`, `{"n":null,"p":null,"m":null}`},
		{"negative zero", `-0`, `0`},
		{"null bytes are escaped, not dropped", `"a\u0000b"`, `"a\u0000b"`},
		{"unicode survives", `"→ שלום"`, `"→ שלום"`},
		{"bidi overrides survive as data", `"a‮b"`, `"a‮b"`},
		{"lone surrogates are escaped", `"\uD800"`, `"\ud800"`},
		{"nested structures", `({a:[1,{b:"c"}],d:true})`, `{"a":[1,{"b":"c"}],"d":true}`},
		{"Date marshals as ISO", `new Date(0)`, `"1970-01-01T00:00:00.000Z"`},
		{"Map is not magically serialised", `new Map([[1,2]])`, `{}`},
		{"toJSON is honoured", `({toJSON(){ return "flat" }})`, `"flat"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := mustEval(t, sb, tc.code)
			if string(res.Value) != tc.want {
				t.Fatalf("%s = %s, want %s", tc.code, res.Value, tc.want)
			}
			if !json.Valid(res.Value) {
				t.Fatalf("%s produced invalid JSON: %s", tc.code, res.Value)
			}
		})
	}

	for _, tc := range []struct {
		name string
		code string
		want string
	}{
		{"a cycle", `const a={}; a.self=a; a`, "circular"},
		{"a function", `(function f(){})`, "returned a function"},
		{"an arrow function", `(() => 1)`, "returned a function"},
		{"a symbol", `Symbol("s")`, "returned a symbol"},
		{"a BigInt", `1n`, "BigInt"},
		{"a cycle through an array", `const a=[]; a.push(a); a`, "circular"},
		// The V1 exclusion a model is most likely to trip over. JSON.stringify
		// would answer "{}" — a silent wrong answer.
		{"a Promise", `(async function(){ return 1 })()`, "no event loop"},
		{"an explicit Promise", `Promise.resolve(1)`, "no event loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eval(t, sb, tc.code, 0)
			if err == nil {
				t.Fatalf("%s crossed the JSON boundary", tc.code)
			}
			if !errors.Is(err, ErrNotJSON) {
				t.Fatalf("error = %v, want ErrNotJSON", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), severityRecoverable) {
				t.Errorf("error carries no severity marker: %q", err)
			}
		})
	}
}

// --- The output cap ---------------------------------------------------------

func TestUnit_Sandbox_OutputCapWithTruncationMarker(t *testing.T) {
	sb := newTestSandbox(t)

	res := mustEval(t, sb, fmt.Sprintf(`"x".repeat(%d)`, DefaultOutputCap*2))
	if !res.Truncated {
		t.Fatal("an over-cap result was not marked truncated")
	}
	if res.Notice == "" || !strings.Contains(res.Notice, "truncated") {
		t.Fatalf("notice = %q, want an explicit truncation marker", res.Notice)
	}
	// The envelope must stay valid JSON even when the payload was cut mid-value.
	if !json.Valid(res.Value) {
		t.Fatalf("truncated result is not valid JSON: %.80s", res.Value)
	}
	var text string
	if err := json.Unmarshal(res.Value, &text); err != nil {
		t.Fatalf("truncated result is not a JSON string: %v", err)
	}
	if !strings.HasSuffix(text, res.Notice) {
		t.Error("the truncated value does not end with the marker")
	}
	if len(res.Value) > DefaultOutputCap+len(res.Notice)+64 {
		t.Errorf("truncated result is %d bytes, over the %d-byte cap", len(res.Value), DefaultOutputCap)
	}

	// A huge string built from multibyte runes must not be cut mid-rune.
	res = mustEval(t, sb, fmt.Sprintf(`"→".repeat(%d)`, DefaultOutputCap))
	if !res.Truncated {
		t.Fatal("an over-cap multibyte result was not marked truncated")
	}
	if !json.Valid(res.Value) {
		t.Fatalf("multibyte truncation produced invalid JSON: %.80s", res.Value)
	}

	// Exactly-at-cap results are not touched.
	res = mustEval(t, sb, `"under the cap"`)
	if res.Truncated || res.Notice != "" {
		t.Error("a small result was marked truncated")
	}
}

// --- The program cache ------------------------------------------------------

func TestUnit_Sandbox_ProgramCacheHitsAndMisses(t *testing.T) {
	sb := newTestSandbox(t)
	const src = `1 + 1`

	h0, m0, _ := sb.cache.stats()
	mustEval(t, sb, src)
	h1, m1, _ := sb.cache.stats()
	if m1 != m0+1 || h1 != h0 {
		t.Fatalf("first compile: hits %d->%d misses %d->%d, want one miss", h0, h1, m0, m1)
	}

	mustEval(t, sb, src)
	mustEval(t, sb, src)
	h2, m2, _ := sb.cache.stats()
	if m2 != m1 || h2 != h1+2 {
		t.Fatalf("repeat compiles: hits %d->%d misses %d->%d, want two hits", h1, h2, m1, m2)
	}

	// Keyed by source, so whitespace is a different program.
	mustEval(t, sb, `1 +  1`)
	_, m3, _ := sb.cache.stats()
	if m3 != m2+1 {
		t.Fatalf("a different source did not miss: misses %d->%d", m2, m3)
	}

	// Bounded: the LRU evicts rather than growing without limit.
	for i := 0; i < programCacheEntries*2; i++ {
		mustEval(t, sb, fmt.Sprintf(`%d`, i))
	}
	if _, _, size := sb.cache.stats(); size > programCacheEntries {
		t.Fatalf("cache holds %d entries, over the %d bound", size, programCacheEntries)
	}
}

// --- Compilation and source limits ------------------------------------------

func TestUnit_Sandbox_SyntaxErrorTeaches(t *testing.T) {
	sb := newTestSandbox(t)

	_, err := eval(t, sb, `function ( {`, 0)
	if err == nil {
		t.Fatal("a syntactically broken program compiled")
	}
	if !strings.Contains(err.Error(), "did not parse") {
		t.Fatalf("error = %q, want the parse failure named", err)
	}
	if !strings.Contains(err.Error(), severityRecoverable) {
		t.Errorf("no severity marker: %q", err)
	}
}

func TestUnit_Sandbox_SourceSizeIsCapped(t *testing.T) {
	sb := newTestSandbox(t)

	_, err := eval(t, sb, "//"+strings.Repeat("x", maxSourceBytes), 0)
	if err == nil {
		t.Fatal("an over-size source compiled")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(maxSourceBytes)) {
		t.Fatalf("error does not name the %d-byte limit: %q", maxSourceBytes, err)
	}
}

// --- Cancellation and shutdown ----------------------------------------------

func TestUnit_Sandbox_CallerCancellationInterrupts(t *testing.T) {
	sb := newTestSandbox(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := sb.runSource(ctx, ToolEval, `while(true){}`, 20*time.Second)
	if err == nil {
		t.Fatal("a cancelled execution returned successfully")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation took %s to land", time.Since(start))
	}
}

func TestUnit_Sandbox_ShutdownRefusesAndInterrupts(t *testing.T) {
	ts, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ts.sb.runSource(context.Background(), ToolEval, `while(true){}`, 20*time.Second)
		done <- err
	}()
	// Give the execution time to actually enter the VM.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	ts.Shutdown()
	if elapsed := time.Since(start); elapsed > shutdownGrace {
		t.Fatalf("Shutdown took %s, over its %s grace", elapsed, shutdownGrace)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrShutdown) {
			t.Fatalf("in-flight execution ended with %v, want ErrShutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight execution was never interrupted by Shutdown")
	}

	// Nothing runs afterwards, and the refusal is typed.
	if _, err := ts.sb.runSource(context.Background(), ToolEval, `1`, time.Second); !errors.Is(err, ErrShutdown) {
		t.Fatalf("post-shutdown execution error = %v, want ErrShutdown", err)
	}
	ts.Shutdown() // idempotent
}

// --- console ----------------------------------------------------------------

func TestUnit_Sandbox_ConsoleIsCapturedAndBounded(t *testing.T) {
	sb := newTestSandbox(t)

	res := mustEval(t, sb, `console.log("a", 1, {b:2}); console.warn("careful"); "done"`)
	if len(res.Logs) != 2 {
		t.Fatalf("logs = %v, want two lines", res.Logs)
	}
	if res.Logs[0] != `a 1 {"b":2}` {
		t.Errorf("log line = %q", res.Logs[0])
	}
	if res.Logs[1] != "warn: careful" {
		t.Errorf("warn line = %q", res.Logs[1])
	}

	// A logging loop cannot spend the context window through the back door.
	res = mustEval(t, sb, `for (let i=0;i<10000;i++) console.log("line "+i); 1`)
	if !res.LogsTruncated {
		t.Fatal("an unbounded logging loop was not truncated")
	}
	if len(res.Logs) > maxLogLines {
		t.Fatalf("kept %d log lines, over the %d cap", len(res.Logs), maxLogLines)
	}
	total := 0
	for _, l := range res.Logs {
		total += len(l)
		if len(l) > maxLogLineBytes+8 {
			t.Fatalf("a log line is %d bytes, over the %d cap", len(l), maxLogLineBytes)
		}
	}
	if total > maxLogBytes {
		t.Fatalf("logs total %d bytes, over the %d cap", total, maxLogBytes)
	}

	// A cyclic object logs rather than failing the call.
	res = mustEval(t, sb, `const a={}; a.self=a; console.log(a); 1`)
	if len(res.Logs) != 1 {
		t.Fatalf("logging a cyclic object produced %v", res.Logs)
	}
}

// --- V1 exclusions ----------------------------------------------------------

// The blueprint excludes the event loop, and an exclusion nobody asserts is a
// feature that creeps back in. These are the observable consequences.
func TestUnit_Sandbox_V1ExclusionsHold(t *testing.T) {
	sb := newTestSandbox(t)

	for _, missing := range []string{"setTimeout", "setInterval", "require", "fetch", "process", "XMLHttpRequest", "importScripts"} {
		res := mustEval(t, sb, fmt.Sprintf(`typeof %s`, missing))
		if string(res.Value) != `"undefined"` {
			t.Errorf("%s is defined in the sandbox (%s) — the sandbox has ambient I/O it should not have", missing, res.Value)
		}
	}

	// No filesystem, no network, no module system: the failure is a plain
	// ReferenceError at the line that tried, not a silent no-op.
	if _, err := eval(t, sb, `require("fs")`, 0); err == nil {
		t.Fatal("require resolved")
	}
}
