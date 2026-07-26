package localtools

// Internal tests for the S8 tool-output filter engine: transform pipeline
// order, XOR rejection, rune-safe truncation, parser 3-tier degradation,
// fail-safe load postures, precedence/override, live kill switch, validator.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func mustCompileFilter(t *testing.T, spec FilterSpec) *compiledFilter {
	t.Helper()
	cf, err := compileFilterSpec(spec, "test")
	require.NoError(t, err)
	return cf
}

// The pipeline runs in its fixed order regardless of author intent:
// ANSI-strip must happen before substitutions (the sub only matches with the
// escapes gone), substitutions before the drop-list (the sub rewrites a line
// INTO the drop-list's pattern), and windows/caps after all of that.
func TestUnit_FilterPipeline_FixedOrder(t *testing.T) {
	cf := mustCompileFilter(t, FilterSpec{
		Name:    "order",
		Command: ".*",
		Substitutions: []FilterSubstitution{
			{Pattern: "^SECRETLEVEL: .*$", Replace: "NOISE: redacted"},
		},
		Drop: []string{"^NOISE: "},
	})
	// The ANSI escapes wrap the marker the substitution anchors on; only the
	// order strip → sub → drop removes both lines.
	in := "\x1b[31mSECRETLEVEL: verbose detail\x1b[0m\nNOISE: progress 42%\nreal error: boom"
	got := cf.applyPipeline(in, 1)
	require.Equal(t, "real error: boom", got)
}

func TestUnit_FilterPipeline_SuccessCollapseWithUnlessGuard(t *testing.T) {
	cf := mustCompileFilter(t, FilterSpec{
		Name:            "collapse",
		Command:         ".*",
		SuccessCollapse: &FilterSuccessCollapse{Message: "all good", Unless: "(?i)warn"},
	})
	require.Equal(t, "all good", cf.applyPipeline("lots\nof\nnoise", 0), "clean success collapses")
	require.Equal(t, "noise\nWARNING: careful", cf.applyPipeline("noise\nWARNING: careful", 0),
		"the unless-guard must veto the collapse")
	require.Equal(t, "lots\nof\nnoise", cf.applyPipeline("lots\nof\nnoise", 1),
		"a failing exit must never collapse")
}

func TestUnit_FilterPipeline_WindowsCapsAndOnEmpty(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, strings.Repeat("x", 5))
	}
	cf := mustCompileFilter(t, FilterSpec{
		Name: "win", Command: ".*", HeadLines: 2, TailLines: 3,
	})
	got := strings.Split(cf.applyPipeline(strings.Join(lines, "\n"), 0), "\n")
	require.Len(t, got, 6, "2 head + marker + 3 tail")
	require.Contains(t, got[2], `15 lines elided by filter "win"`)

	capped := mustCompileFilter(t, FilterSpec{Name: "cap", Command: ".*", MaxLines: 5})
	gotCap := strings.Split(capped.applyPipeline(strings.Join(lines, "\n"), 0), "\n")
	require.Len(t, gotCap, 5, "absolute line cap includes the marker line")
	require.Contains(t, strings.Join(gotCap, "\n"), "elided by filter")

	empty := mustCompileFilter(t, FilterSpec{
		Name: "empty", Command: ".*", Drop: []string{".*"}, OnEmpty: "(nothing survived)",
	})
	require.Equal(t, "(nothing survived)", empty.applyPipeline("a\nb\nc", 0))
}

// Rune safety: the per-line cap and the head/tail byte split must never cut a
// multibyte rune (the repo's multibyte-panic tuition).
func TestUnit_FilterPipeline_RuneSafeTruncation(t *testing.T) {
	line := strings.Repeat("日本語テスト", 50) // 300 runes, 900 bytes
	cf := mustCompileFilter(t, FilterSpec{Name: "runes", Command: ".*", MaxLineLength: 10})
	got := cf.applyPipeline(line, 0)
	require.True(t, utf8.ValidString(got), "per-line cap must cut at rune boundaries")
	require.True(t, strings.HasPrefix(got, "日本語テスト日本語テ"), "exactly 10 runes kept")
	require.Contains(t, got, "[…]")

	split := runeSafeHeadTail(strings.Repeat("é", 4000), 100, "spool")
	require.True(t, utf8.ValidString(split), "head/tail split must cut at rune boundaries")
	require.Contains(t, split, "omitted")
}

func TestUnit_FilterConfig_DropAllowXORRejectedAtLoad(t *testing.T) {
	_, err := compileFilterSpec(FilterSpec{
		Name: "both", Command: ".*", Drop: []string{"a"}, Allow: []string{"b"},
	}, "test")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")

	// Through the loader: the invalid filter is skipped and reported; the
	// valid sibling still loads (fail-safe: malformed file contributes its
	// valid entries).
	cfg := loadFilterConfigBytes([]byte(`{
		"filters": [
			{"name": "bad", "command": ".*", "drop": ["a"], "allow": ["b"]},
			{"name": "broken-regex", "command": "(["},
			{"name": "good", "command": "^echo\\b", "drop": ["^noise"]}
		]
	}`), "test")
	require.Empty(t, cfg.loadErr)
	require.Len(t, cfg.filters, 1)
	require.Equal(t, "good", cfg.filters[0].spec.Name)
	require.Len(t, cfg.issues, 2)
}

func TestUnit_FilterConfig_WholeFileMalformedIsFailSafe(t *testing.T) {
	cfg := loadFilterConfigBytes([]byte("not json at all"), "test")
	require.NotEmpty(t, cfg.loadErr)
	require.Empty(t, cfg.filters)
}

func TestUnit_FilterConfig_BuiltinDefaultsLoadCleanAndPassOwnTests(t *testing.T) {
	cfg := loadBuiltinFilterConfig()
	require.Empty(t, cfg.loadErr)
	require.Empty(t, cfg.issues, "embedded defaults must compile without a single skip")
	require.NotEmpty(t, cfg.filters)

	// The builtin inline tests and match-assertions run green through the
	// real pipeline — the validator validating our own defaults.
	e := NewOutputFilterEngine(nil, WithFilterSources())
	report := e.Validate("")
	require.Zero(t, report.Failures(), "builtin defaults must self-validate: %+v", report)
}

// Precedence: an override in a higher-precedence file OUTRANKS a built-in of
// the same name — including its command regex, so the built-in is gone
// entirely, not merely outranked.
func TestUnit_FilterEngine_ProjectOverrideOutranksBuiltin(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project.json")
	require.NoError(t, os.WriteFile(project, []byte(`{
		"filters": [
			{"name": "npm-install", "command": "^never-matches-anything$", "drop": ["^x"]}
		]
	}`), 0o644))

	e := NewOutputFilterEngine(nil, WithFilterSources(project))
	// The builtin npm-install would collapse this; the override redefines the
	// name with a command regex that matches nothing, so npm output is raw.
	oc := e.Apply(context.Background(), dir, "npm install", "added 12 packages in 2s", 0)
	require.False(t, oc.Matched, "the overridden builtin must be fully shadowed")
	require.Equal(t, "added 12 packages in 2s", oc.Output)
}

func TestUnit_FilterEngine_NoMatchAndNilEngineAreRaw(t *testing.T) {
	oc := (*OutputFilterEngine)(nil).Apply(context.Background(), "", "ls -la", "raw text", 0)
	require.False(t, oc.Matched)
	require.Equal(t, "raw text", oc.Output)

	e := NewOutputFilterEngine(nil, WithFilterSources())
	oc = e.Apply(context.Background(), "", "totally-unknown-cmd --flag", "raw text", 0)
	require.False(t, oc.Matched)
	require.Equal(t, "raw text", oc.Output)
}

// The kill switch is LIVE: flipping the config key takes effect on the next
// application without rebuilding the engine.
func TestUnit_FilterEngine_LiveKillSwitch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "filters.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"disabled": false}`), 0o644))

	e := NewOutputFilterEngine(nil, WithFilterSources(cfgPath))
	ctx := context.Background()
	oc := e.Apply(ctx, dir, "npm install", "added 3 packages in 1s", 0)
	require.True(t, oc.Applied, "builtin npm collapse active while enabled")

	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"disabled": true}`), 0o644))
	// Force a visible mtime change even on coarse-grained filesystems.
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(cfgPath, future, future))

	oc = e.Apply(ctx, dir, "npm install", "added 3 packages in 1s", 0)
	require.False(t, oc.Matched, "kill switch must pass output through raw")
	require.Equal(t, "added 3 packages in 1s", oc.Output)

	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"disabled": false}`), 0o644))
	later := future.Add(2 * time.Second)
	require.NoError(t, os.Chtimes(cfgPath, later, later))
	oc = e.Apply(ctx, dir, "npm install", "added 3 packages in 1s", 0)
	require.True(t, oc.Applied, "flipping the switch back re-enables filtering")
}

// ---------------------------------------------------------------------------
// Parser 3-tier degradation.
// ---------------------------------------------------------------------------

func TestUnit_GoTestJSONParser_StructuredTier(t *testing.T) {
	raw := `{"Action":"run","Package":"p","Test":"TestOK"}
{"Action":"output","Package":"p","Test":"TestOK","Output":"=== RUN   TestOK\n"}
{"Action":"output","Package":"p","Test":"TestOK","Output":"    chatty passing log line\n"}
{"Action":"pass","Package":"p","Test":"TestOK","Elapsed":0.01}
{"Action":"run","Package":"p","Test":"TestBad"}
{"Action":"output","Package":"p","Test":"TestBad","Output":"--- FAIL: TestBad (0.00s)\n"}
{"Action":"output","Package":"p","Test":"TestBad","Output":"    bad_test.go:42: got 1, want 2\n"}
{"Action":"fail","Package":"p","Test":"TestBad","Elapsed":0}
{"Action":"output","Package":"p","Output":"FAIL\tp\t0.1s\n"}
{"Action":"fail","Package":"p","Elapsed":0.1}`
	out, tier := parseGoTestJSON(raw, 1)
	require.Equal(t, tierStructured, tier)
	require.Contains(t, out, "--- FAIL: TestBad")
	require.Contains(t, out, "bad_test.go:42: got 1, want 2")
	require.Contains(t, out, "FAIL\tp")
	require.NotContains(t, out, "chatty passing log line", "passing bodies are dropped")
	require.Contains(t, out, "go test: 1 passed, 1 failed, 0 skipped (1 packages).")
}

func TestUnit_GoTestJSONParser_GrepTier(t *testing.T) {
	raw := "=== RUN TestX\nsome text output, not json\n--- FAIL: TestX (0.01s)\n    x_test.go:9:3: boom\nFAIL\nexit status 1"
	out, tier := parseGoTestJSON(raw, 1)
	require.Equal(t, tierGrep, tier)
	require.Contains(t, out, "--- FAIL: TestX")
	require.Contains(t, out, "x_test.go:9:3: boom")
	require.NotContains(t, out, "=== RUN TestX")
}

func TestUnit_GoTestJSONParser_RawTier(t *testing.T) {
	raw := "completely unrecognizable output\nnothing that looks like a test"
	out, tier := parseGoTestJSON(raw, 1)
	require.Equal(t, tierRaw, tier)
	require.Equal(t, raw, out, "a claimed command that cannot be parsed returns raw, never an error")
}

func TestUnit_GolangciLintParser_Tiers(t *testing.T) {
	jsonIn := `{"Issues":[{"FromLinter":"govet","Text":"shadowed variable","Pos":{"Filename":"a.go","Line":10,"Column":2}}]}`
	out, tier := parseGolangciLint(jsonIn, 1)
	require.Equal(t, tierStructured, tier)
	require.Contains(t, out, "a.go:10:2: shadowed variable (govet)")
	require.Contains(t, out, "golangci-lint: 1 issues.")

	out, tier = parseGolangciLint(`{"Issues":[]}`, 0)
	require.Equal(t, tierStructured, tier)
	require.Contains(t, out, "no issues")

	textIn := "level=info msg=\"loading\"\npkg/a.go:3:1: exported func missing comment (revive)\ndone"
	out, tier = parseGolangciLint(textIn, 1)
	require.Equal(t, tierGrep, tier)
	require.Contains(t, out, "pkg/a.go:3:1:")
	require.NotContains(t, out, "done")

	raw := "no recognizable content"
	out, tier = parseGolangciLint(raw, 1)
	require.Equal(t, tierRaw, tier)
	require.Equal(t, raw, out)
}

func TestUnit_TscParser_Tiers(t *testing.T) {
	in := "src/app.ts(12,5): error TS2345: Argument of type 'string' is not assignable.\nsrc/ok.ts compiled\nFound 1 error."
	out, tier := parseTsc(in, 2)
	require.Equal(t, tierStructured, tier)
	require.Contains(t, out, "error TS2345")
	require.Contains(t, out, "Found 1 error")
	require.NotContains(t, out, "src/ok.ts compiled")

	out, tier = parseTsc("weird wrapper: error TS1005 somewhere in prose", 2)
	require.Equal(t, tierGrep, tier)
	require.Contains(t, out, "error TS1005")

	raw := "nothing diagnostic here"
	out, tier = parseTsc(raw, 1)
	require.Equal(t, tierRaw, tier)
	require.Equal(t, raw, out)
}

func TestUnit_FilterParsers_ClaimRouting(t *testing.T) {
	require.NotNil(t, claimingParser("go test -json ./..."))
	require.Equal(t, "go-test-json", claimingParser("go test -count=1 -json ./internal/...").name)
	require.Nil(t, claimingParser("go test ./..."), "go test without -json is not claimed")
	require.Equal(t, "golangci-lint", claimingParser("golangci-lint run --out-format=json ./...").name)
	require.Equal(t, "tsc", claimingParser("npx tsc --noEmit").name)
	require.Nil(t, claimingParser("ls -la"))
}

// A parser that claims a command NEVER falls through to declarative filters,
// even when it resolves to the raw tier.
func TestUnit_FilterEngine_ParserClaimBlocksDeclarative(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "filters.json")
	// A declarative filter that would aggressively collapse go test output.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"filters": [{"name": "grabby", "command": "go test", "success_collapse": {"message": "collapsed!"}}]
	}`), 0o644))
	e := NewOutputFilterEngine(nil, WithFilterSources(cfgPath))

	raw := "unparseable but claimed"
	oc := e.Apply(context.Background(), dir, "go test -json ./...", raw, 0)
	require.Equal(t, "go-test-json", oc.Name)
	require.Equal(t, raw, oc.Output, "parser raw tier must not fall through to the grabby declarative filter")

	// Without -json the parser does not claim, and the declarative filter wins.
	oc = e.Apply(context.Background(), dir, "go test ./...", raw, 0)
	require.Equal(t, "grabby", oc.Name)
	require.Equal(t, "collapsed!", oc.Output)
}

// ---------------------------------------------------------------------------
// Accounting via libtracker.
// ---------------------------------------------------------------------------

type capturingTracker struct {
	mu      sync.Mutex
	changes []map[string]any
}

func (c *capturingTracker) Start(ctx context.Context, op, subject string, kv ...any) (func(error), func(string, any), func()) {
	return func(error) {},
		func(id string, data any) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if m, ok := data.(map[string]any); ok {
				c.changes = append(c.changes, m)
			}
		},
		func() {}
}

func TestUnit_FilterEngine_TracksCharsBeforeAfter(t *testing.T) {
	tr := &capturingTracker{}
	e := NewOutputFilterEngine(tr, WithFilterSources())
	in := "added 120 packages in 9s\n" + strings.Repeat("npm progress line with plenty of installer chatter\n", 40)
	oc := e.Apply(context.Background(), "", "npm install", in, 0)
	require.True(t, oc.Applied)

	require.Len(t, tr.changes, 1)
	rec := tr.changes[0]
	require.Equal(t, "npm-install", rec["filter"])
	require.Equal(t, utf8.RuneCountInString(in), rec["chars_before"])
	require.Equal(t, utf8.RuneCountInString(oc.Output), rec["chars_after"])
	require.Less(t, rec["chars_after"].(int), rec["chars_before"].(int), "savings are measured, not asserted")
}

// ---------------------------------------------------------------------------
// Validator: inline cases and match-assertions with got/want.
// ---------------------------------------------------------------------------

func TestUnit_FilterValidator_GotWantAndAssertions(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "filters.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"filters": [{
			"name": "mk",
			"command": "^make\\b",
			"drop": ["^make\\[\\d+\\]: (Entering|Leaving) directory"],
			"tests": [
				{"name": "passes", "input": "make[1]: Entering directory '/x'\ncc -o a a.c", "want": "cc -o a a.c", "exit_code": 0},
				{"name": "fails on purpose", "input": "cc -o a a.c", "want": "THIS IS NOT THE OUTPUT", "exit_code": 0}
			]
		}],
		"assertions": [
			{"command": "make -j8", "must_match": "mk"},
			{"command": "cmake .", "must_not_match": "mk"},
			{"command": "ls", "must_match": "mk"}
		]
	}`), 0o644))

	e := NewOutputFilterEngine(nil, WithFilterSources(cfgPath))
	report := e.Validate(dir)

	var file *FilterFileReport
	for i := range report.Files {
		if report.Files[i].Origin == cfgPath {
			file = &report.Files[i]
		}
	}
	require.NotNil(t, file)

	require.Len(t, file.Cases, 2)
	require.True(t, file.Cases[0].Pass)
	require.False(t, file.Cases[1].Pass)
	require.Equal(t, "cc -o a a.c", file.Cases[1].Got, "the failing case reports got")
	require.Equal(t, "THIS IS NOT THE OUTPUT", file.Cases[1].Want, "and want")

	require.Len(t, file.Assertions, 3)
	require.True(t, file.Assertions[0].Pass, "make -j8 must hit mk")
	require.True(t, file.Assertions[1].Pass, "cmake must not hit mk")
	require.False(t, file.Assertions[2].Pass, "ls does not hit mk — assertion fails")
	require.Equal(t, "", file.Assertions[2].Got)

	require.Equal(t, 2, report.Failures(), "one failing case + one failing assertion")
}

func TestUnit_FilterValidator_MissingExplicitFileFailsCI(t *testing.T) {
	e := NewOutputFilterEngine(nil, WithFilterSources(filepath.Join(t.TempDir(), "nope.json")))
	report := e.Validate("")
	require.NotZero(t, report.Failures(), "a typo'd config path must fail, not vanish")
}
