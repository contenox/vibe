package toolguidance_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/services/toolguidance"
	"github.com/getkin/kin-openapi/openapi3"
)

// stubRepo is a minimal ToolsRepo whose Exec returns a fixed result.
type stubRepo struct {
	result any
	dt     taskengine.DataType
	err    error
}

func (s *stubRepo) Exec(_ context.Context, _ time.Time, _ any, _ bool, _ *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	return s.result, s.dt, s.err
}
func (s *stubRepo) Supports(context.Context) ([]string, error) { return []string{"stub"}, nil }
func (s *stubRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}
func (s *stubRepo) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

func stringRepo(s string) *stubRepo { return &stubRepo{result: s, dt: taskengine.DataTypeString} }

// execStr runs the wrapped repo once and returns the result as a string.
func execStr(t *testing.T, r taskengine.ToolsRepo, ctx context.Context, input any, call *taskengine.ToolsCall) string {
	t.Helper()
	res, _, err := r.Exec(ctx, time.Now(), input, false, call)
	if err != nil {
		t.Fatalf("unexpected exec error: %v", err)
	}
	s, ok := res.(string)
	if !ok {
		t.Fatalf("expected string result, got %T", res)
	}
	return s
}

func countHarness(s string) int { return strings.Count(s, "[harness] ") }

func fsCall(tool string) *taskengine.ToolsCall {
	return &taskengine.ToolsCall{Name: "local_fs", ToolName: tool}
}

// TestUnit_ToolGuidance_RepeatMarker_And_FingerprintStability pins the repeat-marker threshold and fingerprint stability across arg shapes and policy keys.
func TestUnit_ToolGuidance_RepeatMarker_And_FingerprintStability(t *testing.T) {
	r := toolguidance.WrapWith(stringRepo("OK"), toolguidance.Options{RepeatThreshold: 3, ScopeEvery: 1000, RevisitThreshold: 1000})
	ctx := toolguidance.WithSession(context.Background(), "s")

	if got := execStr(t, r, ctx, map[string]any{"path": "."}, fsCall("list_dir")); countHarness(got) != 0 {
		t.Fatalf("call 1: expected no marker, got %q", got)
	}
	if got := execStr(t, r, ctx, map[string]any{"path": "."}, fsCall("list_dir")); countHarness(got) != 0 {
		t.Fatalf("call 2: expected no marker, got %q", got)
	}
	if got := execStr(t, r, ctx, map[string]any{"path": "x"}, fsCall("list_dir")); countHarness(got) != 0 {
		t.Fatalf("different-path call: expected no marker, got %q", got)
	}
	got := execStr(t, r, ctx, nil, &taskengine.ToolsCall{Name: "local_fs", ToolName: "list_dir", Args: map[string]string{"path": "."}})
	want := "[harness] 3rd identical list_dir call this session."
	if !strings.Contains(got, want) {
		t.Fatalf("expected %q in result, got %q", want, got)
	}
	if !strings.HasPrefix(got, "OK\n") {
		t.Fatalf("guidance must be appended after the tool's own output, got %q", got)
	}

	got = execStr(t, r, ctx, map[string]any{"path": ".", "_allowed_dir": "/tmp"}, fsCall("list_dir"))
	if !strings.Contains(got, "[harness] 4th identical list_dir call this session.") {
		t.Fatalf("underscore-prefixed policy arg should be excluded from fingerprint, got %q", got)
	}
}

// TestUnit_ToolGuidance_ScopeCadence_And_PathHeuristic pins the scope line's cadence and its dir-vs-file path heuristic.
func TestUnit_ToolGuidance_ScopeCadence_And_PathHeuristic(t *testing.T) {
	r := toolguidance.WrapWith(stringRepo("OK"), toolguidance.Options{RepeatThreshold: 1000, ScopeEvery: 5, RevisitThreshold: 1000})
	ctx := toolguidance.WithSession(context.Background(), "s")

	paths := []string{"src/f0.go", "src/f1.go", "src/f2.go", "src/a/f3.go"}
	for i, p := range paths {
		if got := execStr(t, r, ctx, map[string]any{"path": p}, fsCall("read_file")); countHarness(got) != 0 {
			t.Fatalf("call %d: expected no scope line yet, got %q", i, got)
		}
	}
	got := execStr(t, r, ctx, map[string]any{"path": "src"}, fsCall("list_dir"))
	want := "[harness] scope so far: 4 files across 2 directories."
	if !strings.Contains(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}

	r2 := toolguidance.WrapWith(stringRepo("OK"), toolguidance.Options{RepeatThreshold: 1000, ScopeEvery: 1, RevisitThreshold: 1000})
	ctx2 := toolguidance.WithSession(context.Background(), "s2")
	got = execStr(t, r2, ctx2, map[string]any{"command": "ls -la"}, &taskengine.ToolsCall{Name: "local_shell", ToolName: "run"})
	if !strings.Contains(got, "[harness] scope so far: 0 files across 0 directories.") {
		t.Fatalf("path-less call should contribute nothing to scope, got %q", got)
	}
}

// TestUnit_ToolGuidance_RevisitHint pins that a write does not count as a read and the Nth read fires the hint.
func TestUnit_ToolGuidance_RevisitHint(t *testing.T) {
	r := toolguidance.WrapWith(stringRepo("contents"), toolguidance.Options{RepeatThreshold: 1000, ScopeEvery: 1000, RevisitThreshold: 4})
	ctx := toolguidance.WithSession(context.Background(), "s")

	execStr(t, r, ctx, map[string]any{"path": "a.go", "content": "x"}, fsCall("write_file"))
	for i := 0; i < 3; i++ {
		if got := execStr(t, r, ctx, map[string]any{"path": "a.go"}, fsCall("read_file")); countHarness(got) != 0 {
			t.Fatalf("read %d: expected no revisit yet, got %q", i+1, got)
		}
	}
	got := execStr(t, r, ctx, map[string]any{"path": "a.go"}, fsCall("read_file"))
	want := "[harness] 4th read of a.go this session."
	if !strings.Contains(got, want) {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// TestUnit_ToolGuidance_TwoLineCap pins that at most two lines are emitted, with the scope line dropped first.
func TestUnit_ToolGuidance_TwoLineCap(t *testing.T) {
	r := toolguidance.WrapWith(stringRepo("OK"), toolguidance.Options{RepeatThreshold: 1, ScopeEvery: 1, RevisitThreshold: 1})
	ctx := toolguidance.WithSession(context.Background(), "s")

	got := execStr(t, r, ctx, map[string]any{"path": "a.go"}, fsCall("read_file"))
	if n := countHarness(got); n != 2 {
		t.Fatalf("expected exactly 2 guidance lines (cap), got %d in %q", n, got)
	}
	if strings.Contains(got, "scope so far") {
		t.Fatalf("scope line should be dropped under the two-line cap, got %q", got)
	}
	if !strings.Contains(got, "identical read_file") || !strings.Contains(got, "read of a.go") {
		t.Fatalf("expected repeat + revisit lines, got %q", got)
	}
}

// TestUnit_ToolGuidance_ErrorResultsUntouched pins that an errored call passes through verbatim and is never counted.
func TestUnit_ToolGuidance_ErrorResultsUntouched(t *testing.T) {
	sentinel := errString("boom")
	inner := &togglingRepo{}
	r := toolguidance.WrapWith(inner, toolguidance.Options{RepeatThreshold: 2, ScopeEvery: 1000, RevisitThreshold: 1000})
	ctx := toolguidance.WithSession(context.Background(), "s")

	inner.err = sentinel
	res, _, err := r.Exec(ctx, time.Now(), map[string]any{"path": "."}, false, fsCall("list_dir"))
	if err != sentinel {
		t.Fatalf("error must pass through verbatim, got %v", err)
	}
	if res != nil {
		t.Fatalf("errored result must be untouched (nil), got %v", res)
	}

	inner.err = nil
	if got := execStr(t, r, ctx, map[string]any{"path": "."}, fsCall("list_dir")); countHarness(got) != 0 {
		t.Fatalf("success 1: expected no marker (error must not have counted), got %q", got)
	}
	if got := execStr(t, r, ctx, map[string]any{"path": "."}, fsCall("list_dir")); !strings.Contains(got, "2nd identical") {
		t.Fatalf("success 2: expected 2nd-identical marker, got %q", got)
	}
}

// TestUnit_ToolGuidance_JSONResultsUntouched pins that a JSON result is returned byte-for-byte, never turned into a string.
func TestUnit_ToolGuidance_JSONResultsUntouched(t *testing.T) {
	type payload struct {
		Written bool `json:"written"`
	}
	jsonRepo := &stubRepo{result: payload{Written: true}, dt: taskengine.DataTypeJSON}
	r := toolguidance.WrapWith(jsonRepo, toolguidance.Options{RepeatThreshold: 2, ScopeEvery: 1, RevisitThreshold: 1000})
	ctx := toolguidance.WithSession(context.Background(), "s")

	res, dt, err := r.Exec(ctx, time.Now(), map[string]any{"path": "a.go", "content": "x"}, false, fsCall("write_file"))
	if err != nil {
		t.Fatal(err)
	}
	if dt != taskengine.DataTypeJSON {
		t.Fatalf("data type must be preserved, got %v", dt)
	}
	if got, ok := res.(payload); !ok || !got.Written {
		t.Fatalf("JSON result shape corrupted: %#v", res)
	}
}

// TestUnit_ToolGuidance_JSONCountedButNotSurfaced pins that a JSON call still advances the shared counters.
func TestUnit_ToolGuidance_JSONCountedButNotSurfaced(t *testing.T) {
	inner := &shapeRepo{}
	r := toolguidance.WrapWith(inner, toolguidance.Options{RepeatThreshold: 2, ScopeEvery: 1000, RevisitThreshold: 1000})
	ctx := toolguidance.WithSession(context.Background(), "s")

	inner.json = true
	if _, dt, _ := r.Exec(ctx, time.Now(), map[string]any{"path": "p"}, false, fsCall("thing")); dt != taskengine.DataTypeJSON {
		t.Fatalf("expected JSON on first call")
	}
	inner.json = false
	got := execStr(t, r, ctx, map[string]any{"path": "p"}, fsCall("thing"))
	if !strings.Contains(got, "2nd identical thing call this session.") {
		t.Fatalf("JSON call should have counted toward repeat, got %q", got)
	}
}

// TestUnit_ToolGuidance_PerSessionIsolationAndReset pins that distinct sessions, and distinct per-turn request ids, never share counters.
func TestUnit_ToolGuidance_PerSessionIsolationAndReset(t *testing.T) {
	r := toolguidance.WrapWith(stringRepo("OK"), toolguidance.Options{RepeatThreshold: 3, ScopeEvery: 1000, RevisitThreshold: 1000})

	s1 := toolguidance.WithSession(context.Background(), "s1")
	s2 := toolguidance.WithSession(context.Background(), "s2")

	execStr(t, r, s1, map[string]any{"path": "."}, fsCall("list_dir"))
	execStr(t, r, s1, map[string]any{"path": "."}, fsCall("list_dir"))
	if got := execStr(t, r, s1, map[string]any{"path": "."}, fsCall("list_dir")); !strings.Contains(got, "3rd identical") {
		t.Fatalf("s1 should hit threshold, got %q", got)
	}
	execStr(t, r, s2, map[string]any{"path": "."}, fsCall("list_dir"))
	if got := execStr(t, r, s2, map[string]any{"path": "."}, fsCall("list_dir")); countHarness(got) != 0 {
		t.Fatalf("s2 must be isolated from s1, got %q", got)
	}

	turnA := libtracker.WithNewRequestID(context.Background())
	turnB := libtracker.WithNewRequestID(context.Background())
	execStr(t, r, turnA, map[string]any{"path": "z"}, fsCall("list_dir"))
	execStr(t, r, turnA, map[string]any{"path": "z"}, fsCall("list_dir"))
	if got := execStr(t, r, turnB, map[string]any{"path": "z"}, fsCall("list_dir")); countHarness(got) != 0 {
		t.Fatalf("distinct turns must not share counters, got %q", got)
	}
}

// TestUnit_ToolGuidance_Concurrency pins that concurrent calls on one session never lose an increment.
func TestUnit_ToolGuidance_Concurrency(t *testing.T) {
	r := toolguidance.WrapWith(stringRepo("OK"), toolguidance.Options{RepeatThreshold: 1, ScopeEvery: 7, RevisitThreshold: 1})
	ctx := toolguidance.WithSession(context.Background(), "race")

	const n = 60
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = r.Exec(ctx, time.Now(), map[string]any{"path": "a.go"}, false, fsCall("read_file"))
		}()
	}
	other := toolguidance.WithSession(context.Background(), "race2")
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = r.Exec(other, time.Now(), map[string]any{"path": "b.go"}, false, fsCall("read_file"))
		}()
	}
	wg.Wait()

	got := execStr(t, r, ctx, map[string]any{"path": "a.go"}, fsCall("read_file"))
	wantOrd := ordinalStr(n + 1)
	if !strings.Contains(got, wantOrd+" identical read_file call this session.") {
		t.Fatalf("expected %s-identical after %d concurrent calls, got %q", wantOrd, n, got)
	}
}

// TestUnit_ToolGuidance_OffSwitch pins that an off-ish env value returns the inner repo untouched (identity).
func TestUnit_ToolGuidance_OffSwitch(t *testing.T) {
	inner := stringRepo("OK")

	t.Setenv("CONTENOX_TOOL_GUIDANCE", "")
	if !toolguidance.Enabled() {
		t.Fatal("expected enabled by default")
	}
	wrapped := toolguidance.WrapFromEnv(inner)
	if any(wrapped) == any(inner) {
		t.Fatal("expected a real wrapper when enabled")
	}

	for _, v := range []string{"off", "false", "0", "no", "disabled"} {
		t.Setenv("CONTENOX_TOOL_GUIDANCE", v)
		if toolguidance.Enabled() {
			t.Fatalf("value %q should disable", v)
		}
		if got := toolguidance.WrapFromEnv(inner); any(got) != any(inner) {
			t.Fatalf("value %q: expected inner repo returned untouched", v)
		}
	}
}

// --- helpers ---

// errString is a tiny error type so the sentinel-error identity check is exact.
type errString string

func (e errString) Error() string { return string(e) }

// shapeRepo returns JSON or string based on its json flag.
type shapeRepo struct{ json bool }

func (s *shapeRepo) Exec(_ context.Context, _ time.Time, _ any, _ bool, _ *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if s.json {
		return map[string]any{"ok": true}, taskengine.DataTypeJSON, nil
	}
	return "OK", taskengine.DataTypeString, nil
}
func (s *shapeRepo) Supports(context.Context) ([]string, error) { return []string{"stub"}, nil }
func (s *shapeRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}
func (s *shapeRepo) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

// togglingRepo returns a settable error and otherwise a string result.
type togglingRepo struct{ err error }

func (s *togglingRepo) Exec(_ context.Context, _ time.Time, _ any, _ bool, _ *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if s.err != nil {
		return nil, taskengine.DataTypeAny, s.err
	}
	return "OK", taskengine.DataTypeString, nil
}
func (s *togglingRepo) Supports(context.Context) ([]string, error) { return []string{"stub"}, nil }
func (s *togglingRepo) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}
func (s *togglingRepo) GetToolsForToolsByName(context.Context, string) ([]taskengine.Tool, error) {
	return nil, nil
}

// ordinalStr mirrors the package's ordinal formatting for test expectations.
func ordinalStr(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return itoa(n) + "th"
	}
	switch n % 10 {
	case 1:
		return itoa(n) + "st"
	case 2:
		return itoa(n) + "nd"
	case 3:
		return itoa(n) + "rd"
	default:
		return itoa(n) + "th"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// carrierResult is a typed tool result that renders as text.
type carrierResult struct {
	Field string `json:"field"`
	text  string
}

func (c carrierResult) String() string { return c.text }

func (c carrierResult) AppendGuidance(text string) any {
	c.text += text
	return c
}

// TestUnit_ToolGuidance_TypedResultThatRendersAsTextKeepsItsGuidance pins that a typed AppendGuidance carrier keeps its guidance and its shape.
func TestUnit_ToolGuidance_TypedResultThatRendersAsTextKeepsItsGuidance(t *testing.T) {
	repo := &stubRepo{result: carrierResult{Field: "v", text: "branch main\n"}, dt: taskengine.DataTypeString}
	r := toolguidance.WrapWith(repo, toolguidance.Options{RepeatThreshold: 2, ScopeEvery: 1000, RevisitThreshold: 1000})
	ctx := toolguidance.WithSession(context.Background(), "s")
	call := &taskengine.ToolsCall{Name: "git", ToolName: "git_status"}

	res, _, err := r.Exec(ctx, time.Now(), map[string]any{}, false, call)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got, ok := res.(carrierResult); !ok || got.text != "branch main\n" {
		t.Fatalf("an unmarked result was altered: %#v", res)
	}

	res, _, err = r.Exec(ctx, time.Now(), map[string]any{}, false, call)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	got, ok := res.(carrierResult)
	if !ok {
		t.Fatalf("the decorator replaced a typed result with %T — appending must never change a result's shape", res)
	}
	if countHarness(got.String()) == 0 {
		t.Fatalf("a typed result lost its guidance: %q", got.String())
	}
	if !strings.HasPrefix(got.String(), "branch main\n") {
		t.Fatalf("the tool's own output was not preserved: %q", got.String())
	}
	if got.Field != "v" {
		t.Fatalf("the structured fields were lost: %#v", got)
	}
}

// TestUnit_ToolGuidance_TypedResultWithoutACarrierIsUntouched pins that a non-carrier typed result is returned byte-for-byte.
func TestUnit_ToolGuidance_TypedResultWithoutACarrierIsUntouched(t *testing.T) {
	type plain struct{ A int }
	repo := &stubRepo{result: plain{A: 1}, dt: taskengine.DataTypeJSON}
	r := toolguidance.WrapWith(repo, toolguidance.Options{RepeatThreshold: 1, ScopeEvery: 1, RevisitThreshold: 1})
	ctx := toolguidance.WithSession(context.Background(), "s")

	res, _, err := r.Exec(ctx, time.Now(), map[string]any{}, false, fsCall("write_file"))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got, ok := res.(plain); !ok || got.A != 1 {
		t.Fatalf("a structured result was altered: %#v", res)
	}
}
