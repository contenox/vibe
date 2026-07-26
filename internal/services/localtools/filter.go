package localtools

// filter.go — the tool-output filter engine (TODO.md S8, pando-mining.md item
// 1 / F2-G1): the PREVENTIVE primitive in the context story. It compresses
// noisy stdout from known command shapes BEFORE the inline size cap is
// applied, so compression buys context headroom instead of merely reordering
// truncation.
//
// Resolution order per application:
//  1. live kill switch ("disabled" key, highest-precedence file wins),
//  2. native structured parsers (go test -json, golangci-lint, tsc) — a
//     parser that claims a command owns it: it degrades through tiers
//     (structured → grep → raw) and NEVER falls through to declarative
//     filters,
//  3. declarative filters, first command-regex match wins, with the
//     precedence project-local → user-global → embedded defaults and
//     name-shadowing (an override outranks a built-in),
//  4. no match → raw.
//
// Structural guarantees live at the call site (localexec.go): only stdout is
// ever filtered; stderr, the exit code, and the failure suffix are assembled
// after filtering and are untouchable. The raw stream is always preserved in
// the spool whenever a filter rewrites output, and the result names the
// filter and the spool path. Every application records chars-before/after via
// libtracker so the compression story is measured, not asserted.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/contenox/beam/internal/libtracker"
)

const (
	// filtersUserConfigEnvVar overrides the user-global filter config path
	// (default ~/.contenox/filters.json). Useful for tests and deployments.
	filtersUserConfigEnvVar = "CONTENOX_FILTERS_USER_CONFIG"

	// projectFilterConfigRel is the project-local config location, discovered
	// by walking up from the command's working directory.
	projectFilterConfigRel = ".contenox/filters.json"
)

// FilterOutcome is the result of one engine application.
type FilterOutcome struct {
	Name    string // filter or parser that handled the command ("" = none)
	Tier    string // "structured" | "grep" | "raw" (parsers) or "declarative"
	Output  string // the (possibly rewritten) stdout
	Matched bool   // a filter/parser claimed the command
	Applied bool   // Matched AND the output actually changed
}

// OutputFilterEngine matches commands to filters and runs the fixed transform
// pipeline. A nil engine is valid and passes everything through raw.
type OutputFilterEngine struct {
	tracker libtracker.ActivityTracker

	explicitSet bool
	explicit    []string // explicit source paths, highest precedence first
	userPath    string   // override for the user-global config path

	mu      sync.Mutex
	cache   map[string]*cachedFilterSource
	builtin *loadedFilterConfig
}

type cachedFilterSource struct {
	mtime time.Time
	size  int64
	cfg   *loadedFilterConfig // nil when the file is missing/unreadable
	err   string
}

// FilterEngineOption configures an OutputFilterEngine.
type FilterEngineOption func(*OutputFilterEngine)

// WithFilterSources replaces config discovery with an explicit, ordered list
// of config files (highest precedence first). Zero paths = embedded defaults
// only. Intended for tests and the CLI validator.
func WithFilterSources(paths ...string) FilterEngineOption {
	return func(e *OutputFilterEngine) {
		e.explicitSet = true
		e.explicit = paths
	}
}

// WithFilterUserConfigPath overrides the user-global config path
// (~/.contenox/filters.json by default).
func WithFilterUserConfigPath(path string) FilterEngineOption {
	return func(e *OutputFilterEngine) { e.userPath = path }
}

// NewOutputFilterEngine builds an engine. The tracker records
// chars-before/after per application; pass libtracker.NoopTracker{} to opt
// out of accounting.
func NewOutputFilterEngine(tracker libtracker.ActivityTracker, opts ...FilterEngineOption) *OutputFilterEngine {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	e := &OutputFilterEngine{
		tracker: tracker,
		cache:   map[string]*cachedFilterSource{},
		builtin: loadBuiltinFilterConfig(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Apply runs the engine over one command's stdout. cwd is the command's
// working directory (used for project-local config discovery); exitCode
// gates success-collapse. It never fails: any degenerate state returns the
// input raw.
func (e *OutputFilterEngine) Apply(ctx context.Context, cwd, command, stdout string, exitCode int) FilterOutcome {
	raw := FilterOutcome{Output: stdout}
	if e == nil {
		return raw
	}
	configs := e.effectiveConfigs(cwd)
	if filterKillSwitchOn(configs) {
		return raw
	}
	if p := claimingParser(command); p != nil {
		out, tier := p.parse(stdout, exitCode)
		oc := FilterOutcome{Name: p.name, Tier: tier.String(), Output: out, Matched: true, Applied: out != stdout}
		e.track(ctx, command, stdout, oc)
		return oc
	}
	for _, f := range effectiveFilters(configs) {
		if f.command.MatchString(command) {
			out := f.applyPipeline(stdout, exitCode)
			oc := FilterOutcome{Name: f.spec.Name, Tier: "declarative", Output: out, Matched: true, Applied: out != stdout}
			e.track(ctx, command, stdout, oc)
			return oc
		}
	}
	return raw
}

// track records one application's savings via libtracker (chars measured in
// runes: this is a context-size story, not a byte story).
func (e *OutputFilterEngine) track(ctx context.Context, command, before string, oc FilterOutcome) {
	_, reportChange, end := e.tracker.Start(ctx, "filter", "tool_output",
		"filter", oc.Name, "tier", oc.Tier)
	reportChange(oc.Name, map[string]any{
		"filter":       oc.Name,
		"tier":         oc.Tier,
		"command":      command,
		"chars_before": utf8.RuneCountInString(before),
		"chars_after":  utf8.RuneCountInString(oc.Output),
	})
	end()
}

// effectiveConfigs returns the ordered config chain for this application:
// explicit sources when set, else discovered project-local then user-global;
// the embedded defaults always come last.
func (e *OutputFilterEngine) effectiveConfigs(cwd string) []*loadedFilterConfig {
	var out []*loadedFilterConfig
	if e.explicitSet {
		for _, p := range e.explicit {
			if cfg := e.sourceFor(p); cfg != nil {
				out = append(out, cfg)
			}
		}
	} else {
		if p := discoverProjectFilterConfig(cwd); p != "" {
			if cfg := e.sourceFor(p); cfg != nil {
				out = append(out, cfg)
			}
		}
		if u := e.resolveUserConfigPath(); u != "" {
			if cfg := e.sourceFor(u); cfg != nil {
				out = append(out, cfg)
			}
		}
	}
	if e.builtin != nil {
		out = append(out, e.builtin)
	}
	return out
}

func (e *OutputFilterEngine) resolveUserConfigPath() string {
	if e.userPath != "" {
		return e.userPath
	}
	if v := strings.TrimSpace(os.Getenv(filtersUserConfigEnvVar)); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".contenox", "filters.json")
	}
	return ""
}

// sourceFor loads (or reuses the cached parse of) one config file. The cache
// is invalidated on any mtime/size change, which is what makes the kill
// switch and config edits LIVE: a stat per application, a re-parse only on
// change. Missing or unreadable files return nil — fail-safe.
func (e *OutputFilterEngine) sourceFor(path string) *loadedFilterConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	info, statErr := os.Stat(path)
	if statErr != nil || info.IsDir() {
		e.cache[path] = &cachedFilterSource{err: "not found"}
		return nil
	}
	if c, ok := e.cache[path]; ok && c.cfg != nil && c.mtime.Equal(info.ModTime()) && c.size == info.Size() {
		return c.cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		e.cache[path] = &cachedFilterSource{err: err.Error()}
		return nil
	}
	cfg := loadFilterConfigBytes(data, path)
	e.cache[path] = &cachedFilterSource{mtime: info.ModTime(), size: info.Size(), cfg: cfg}
	return cfg
}

// discoverProjectFilterConfig walks up from cwd looking for
// .contenox/filters.json (the .gitignore discovery idiom).
func discoverProjectFilterConfig(cwd string) string {
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return ""
		}
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, filepath.FromSlash(projectFilterConfigRel))
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// filterKillSwitchOn resolves the live kill switch: the highest-precedence
// config that sets "disabled" wins.
func filterKillSwitchOn(configs []*loadedFilterConfig) bool {
	for _, c := range configs {
		if c.disabled != nil {
			return *c.disabled
		}
	}
	return false
}

// effectiveFilters flattens the config chain into one ordered filter list
// with name-shadowing: a filter name seen in a higher-precedence file hides
// every lower-precedence filter of the same name (an override OUTRANKS a
// built-in — including its command regex).
func effectiveFilters(configs []*loadedFilterConfig) []*compiledFilter {
	var out []*compiledFilter
	seen := map[string]bool{}
	for _, c := range configs {
		for _, f := range c.filters {
			if seen[f.spec.Name] {
				continue
			}
			seen[f.spec.Name] = true
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Transform pipeline — FIXED order, never author-ordered.
// ---------------------------------------------------------------------------

// ansiEscapeRe matches CSI sequences, OSC sequences, and stray two-byte
// escapes — the escape vocabulary progress bars and colored tools emit.
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\^_]`)

func stripANSIEscapes(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiEscapeRe.ReplaceAllString(s, "")
}

// applyPipeline runs the fixed-order transform pipeline over stdout.
func (f *compiledFilter) applyPipeline(stdout string, exitCode int) string {
	out := strings.ReplaceAll(stdout, "\r\n", "\n")

	// 1. strip ANSI.
	if f.stripANSI {
		out = stripANSIEscapes(out)
	}

	// 2. per-line regex substitutions.
	if len(f.subs) > 0 {
		lines := strings.Split(out, "\n")
		for i, line := range lines {
			for _, s := range f.subs {
				line = s.re.ReplaceAllString(line, s.replace)
			}
			lines[i] = line
		}
		out = strings.Join(lines, "\n")
	}

	// 3. whole-output success-collapse (with optional unless-guard). The
	// collapse message is final: later stages must never mangle it.
	if f.spec.SuccessCollapse != nil && exitCode == 0 {
		if f.collapseUnless == nil || !f.collapseUnless.MatchString(out) {
			return f.spec.SuccessCollapse.Message
		}
	}

	lines := strings.Split(out, "\n")

	// 4. drop-list XOR allow-list (both declared is rejected at load).
	switch {
	case len(f.drop) > 0:
		kept := lines[:0]
		for _, line := range lines {
			if !matchesAny(f.drop, line) {
				kept = append(kept, line)
			}
		}
		lines = kept
	case len(f.allow) > 0:
		kept := lines[:0]
		for _, line := range lines {
			if matchesAny(f.allow, line) {
				kept = append(kept, line)
			}
		}
		lines = kept
	}

	// 5. per-line length cap — rune-safe.
	if f.spec.MaxLineLength > 0 {
		for i, line := range lines {
			lines[i] = capLineToRunes(line, f.spec.MaxLineLength)
		}
	}

	// 6. head/tail line windows with an explicit elision marker.
	lines = windowLines(lines, f.spec.HeadLines, f.spec.TailLines, f.spec.Name)

	// 7. absolute line cap.
	lines = capTotalLines(lines, f.spec.MaxLines, f.spec.Name)

	out = strings.Join(lines, "\n")

	// 8. on-empty message.
	if strings.TrimSpace(out) == "" && f.spec.OnEmpty != "" {
		out = f.spec.OnEmpty
	}
	return out
}

func matchesAny(res []*regexp.Regexp, line string) bool {
	for _, re := range res {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// capLineToRunes truncates a line to at most maxRunes runes, cutting only at
// rune boundaries (this repo already paid the multibyte-panic tuition once).
func capLineToRunes(line string, maxRunes int) string {
	if maxRunes <= 0 {
		return line
	}
	count := 0
	for i := range line {
		if count == maxRunes {
			return line[:i] + " […]"
		}
		count++
	}
	return line
}

func elisionMarker(n int, filterName string) string {
	return fmt.Sprintf("... [%d lines elided by filter %q] ...", n, filterName)
}

// windowLines keeps the first `head` and last `tail` lines with an explicit
// elision marker in between. head or tail alone keeps only that window.
func windowLines(lines []string, head, tail int, filterName string) []string {
	if head <= 0 && tail <= 0 {
		return lines
	}
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	if head+tail >= len(lines) {
		return lines
	}
	out := make([]string, 0, head+tail+1)
	out = append(out, lines[:head]...)
	out = append(out, elisionMarker(len(lines)-head-tail, filterName))
	out = append(out, lines[len(lines)-tail:]...)
	return out
}

// capTotalLines enforces the absolute line cap (marker line included),
// keeping a head/tail split so failures at either end survive.
func capTotalLines(lines []string, max int, filterName string) []string {
	if max <= 0 || len(lines) <= max {
		return lines
	}
	if max == 1 {
		return []string{elisionMarker(len(lines), filterName)}
	}
	head := (max - 1) / 2
	tail := max - 1 - head
	out := make([]string, 0, max)
	out = append(out, lines[:head]...)
	out = append(out, elisionMarker(len(lines)-head-tail, filterName))
	out = append(out, lines[len(lines)-tail:]...)
	return out
}

// ---------------------------------------------------------------------------
// local_shell integration glue.
// ---------------------------------------------------------------------------

// filteredStdout is what the local_shell result path needs from a successful
// filter application: the budget-capped inline text, the metadata naming the
// filter, and the notice pointing at the preserved raw stream.
type filteredStdout struct {
	name         string
	tier         string
	inline       string
	notice       string
	rawSpoolPath string
	overBudget   bool
}

// filterShellStdout applies the engine to a completed command's stdout,
// BEFORE the inline size cap: when the filtered text fits the budget the
// truncation posture never triggers (compression bought headroom). It returns
// nil whenever filtering must not happen — nil engine, unretrievable raw
// stream, no match, unchanged output, or a raw stream that could not be
// preserved in the spool. nil means: use the existing unfiltered path.
func filterShellStdout(ctx context.Context, engine *OutputFilterEngine, spec CommandSpec, w *spoolWriter, spoolPath string, exitCode int, budget int64) *filteredStdout {
	if engine == nil || exitCode < 0 {
		return nil
	}
	raw, ok := w.fullText()
	if !ok {
		return nil
	}
	commandLine := spec.Command
	if len(spec.Args) > 0 {
		commandLine += " " + strings.Join(spec.Args, " ")
	}
	oc := engine.Apply(ctx, spec.Cwd, commandLine, raw, exitCode)
	if !oc.Applied {
		return nil
	}
	// OUR improvement over pando's one-artifact design: the raw stream is
	// ALWAYS preserved in the spool when a filter rewrites output, so the
	// human can see what raw looked like. If it cannot be preserved, we do
	// not filter — fail-safe.
	rawPath := spoolPath
	if rawPath == "" {
		var err error
		if rawPath, err = spoolText(ctx, "local_shell-stdout-raw", raw); err != nil {
			return nil
		}
	}
	fs := &filteredStdout{
		name:         oc.Name,
		tier:         oc.Tier,
		inline:       oc.Output,
		rawSpoolPath: rawPath,
	}
	if int64(len(fs.inline)) > budget {
		fs.overBudget = true
		fs.inline = runeSafeHeadTail(fs.inline, budget, "raw output: "+rawPath)
	}
	fs.notice = fmt.Sprintf("stdout filtered by %q (%s): %d chars -> %d chars; raw output preserved: %s",
		oc.Name, oc.Tier, utf8.RuneCountInString(raw), utf8.RuneCountInString(oc.Output), rawPath)
	return fs
}

// spoolText writes text to a fresh spool file (the spool.go idiom) and
// returns its path.
func spoolText(ctx context.Context, tool, text string) (string, error) {
	f, path, err := newSpoolFile(ctx, tool)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.WriteString(f, text); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// runeSafeHeadTail renders a 20%-head/80%-tail split of s within roughly
// `budget` bytes, cutting only at rune boundaries, with an elision marker
// naming where the full text lives.
func runeSafeHeadTail(s string, budget int64, note string) string {
	if budget < 2 {
		budget = 2
	}
	if int64(len(s)) <= budget {
		return s
	}
	headEnd := int(budget / 5)
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - int(budget-budget/5)
	if tailStart < headEnd {
		tailStart = headEnd
	}
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	omitted := int64(tailStart - headEnd)
	return s[:headEnd] + fmt.Sprintf("\n... [%s omitted — %s] ...\n", humanSize(omitted), note) + s[tailStart:]
}

// ---------------------------------------------------------------------------
// Validator — inline test cases and match-assertions through the REAL
// pipeline, surfaced by `contenox tools filter test`.
// ---------------------------------------------------------------------------

// FilterCaseResult is one inline test case run through the real pipeline.
type FilterCaseResult struct {
	Filter string
	Case   string
	Pass   bool
	Got    string
	Want   string
}

// FilterAssertionResult is one routing assertion result.
type FilterAssertionResult struct {
	Command string
	Expect  string // human framing of the assertion
	Got     string // filter/parser that actually claimed the command ("" = none)
	Pass    bool
}

// FilterFileReport aggregates validation results for one config source.
type FilterFileReport struct {
	Origin     string
	LoadError  string // whole-file failure (missing/unparseable)
	Issues     []FilterLoadIssue
	Cases      []FilterCaseResult
	Assertions []FilterAssertionResult
}

// FilterValidationReport is the full validator output.
type FilterValidationReport struct {
	Disabled bool // the kill switch is currently on (validation still ran)
	Files    []FilterFileReport
}

// Failures counts everything that should fail CI: load errors, skipped
// entries, failing cases, failing assertions.
func (r FilterValidationReport) Failures() int {
	n := 0
	for _, f := range r.Files {
		if f.LoadError != "" {
			n++
		}
		n += len(f.Issues)
		for _, c := range f.Cases {
			if !c.Pass {
				n++
			}
		}
		for _, a := range f.Assertions {
			if !a.Pass {
				n++
			}
		}
	}
	return n
}

// Validate runs every inline test case and match-assertion in the effective
// config chain (explicit sources when configured, else discovery from cwd,
// plus the embedded defaults) through the real pipeline. The kill switch is
// bypassed for validation — configs must stay testable while disabled — but
// its state is reported.
func (e *OutputFilterEngine) Validate(cwd string) FilterValidationReport {
	configs := e.effectiveConfigs(cwd)
	report := FilterValidationReport{Disabled: filterKillSwitchOn(configs)}

	// Explicitly-listed files that failed to load entirely still get a report
	// entry, so a typo'd path or broken JSON fails CI instead of vanishing.
	if e.explicitSet {
		loadedOrigins := map[string]bool{}
		for _, c := range configs {
			loadedOrigins[c.origin] = true
		}
		for _, p := range e.explicit {
			if !loadedOrigins[p] {
				report.Files = append(report.Files, FilterFileReport{Origin: p, LoadError: "config file not found or unreadable"})
			}
		}
	}

	filters := effectiveFilters(configs)
	shadowed := func(f *compiledFilter) bool {
		for _, ef := range filters {
			if ef == f {
				return false
			}
		}
		return true
	}

	for _, c := range configs {
		fr := FilterFileReport{Origin: c.origin, LoadError: c.loadErr, Issues: c.issues}
		for _, f := range c.filters {
			if shadowed(f) {
				continue // an override outranks it; its cases belong to the override
			}
			for i, tc := range f.spec.Tests {
				name := tc.Name
				if name == "" {
					name = fmt.Sprintf("case %d", i+1)
				}
				got := f.applyPipeline(tc.Input, tc.ExitCode)
				fr.Cases = append(fr.Cases, FilterCaseResult{
					Filter: f.spec.Name,
					Case:   name,
					Pass:   got == tc.Want,
					Got:    got,
					Want:   tc.Want,
				})
			}
		}
		for _, a := range c.assertions {
			got := resolveFilterMatch(filters, a.Command)
			ar := FilterAssertionResult{Command: a.Command, Got: got}
			if a.MustMatch != "" {
				ar.Expect = fmt.Sprintf("must hit %q", a.MustMatch)
				ar.Pass = got == a.MustMatch
			} else {
				ar.Expect = fmt.Sprintf("must NOT hit %q", a.MustNotMatch)
				ar.Pass = got != a.MustNotMatch
			}
			fr.Assertions = append(fr.Assertions, ar)
		}
		report.Files = append(report.Files, fr)
	}
	return report
}

// resolveFilterMatch mirrors Apply's routing (parsers first, then the
// shadow-resolved declarative list) and returns the name that would claim the
// command, or "".
func resolveFilterMatch(filters []*compiledFilter, command string) string {
	if p := claimingParser(command); p != nil {
		return p.name
	}
	for _, f := range filters {
		if f.command.MatchString(command) {
			return f.spec.Name
		}
	}
	return ""
}
