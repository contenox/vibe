// Package toolguidance is the attention layer's Stage 0 — the inward face.
//
// Two conclusions about code navigation drive this
// package, turned inward from a human editing code to a MODEL navigating a repo
// through tools:
//
//   - The blind-spot doctrine: developers call bookmarks
//     useful and never set them. Models are the same, only worse — they cannot
//     judge the navigation value of their own tool calls and will never curate a
//     navigation memory. So orientation must be an automatic by-product of the
//     work. Here it is derived, never asked for: a deterministic counter over
//     (tool, path, args-fingerprint) that the harness maintains and the model
//     merely reads.
//   - The metric lesson: navigation COUNT
//     is not the signal; SCOPE is — how few paths a unit touched, how fast it
//     abandoned a wrong one. Corollary: derailment is a scope anomaly before it
//     is anything else (the first real derailed fleet unit, wandering $HOME
//     instead of the repo, was legible from its first two tool calls). This
//     package emits the same signal the reviewer will later rank on, fed back to
//     the agent LIVE through the one channel every model reads: the tool-result
//     envelope.
//
// # The law this package must not break — advice, never a gate
//
// Stage 0 APPENDS to a tool's textual result; it never modifies the tool's own
// output, never changes a result's shape, never fails a call, and never blocks
// one. The attention layer ranks and flags; ENVELOPES gate (tool-hardening.md).
// A guidance line is a note in the margin, not a veto — the exoskeleton, not the
// autopilot. Every decision below (append-only, error-results-untouched,
// two-line cap, non-string transparency) is that one law made mechanical.
//
// # The dial this package is waiting for — ModelProfile
//
// The guidance envelope is a tool-hardening surface: terse, fixed-format,
// clearly non-content. On a flagship model these lines are cheap orientation; on
// a weak local model the same lines are noise that can derail the very
// navigation they describe (tool-hardening.md's "works on Gemini, degrades
// elsewhere"). The eventual home for that trade-off is the ModelProfile table
// (rec 1): tool_guidance becomes a per-model dial — verbosity, thresholds, or
// off — defaulting to ON, exactly as description-verbosity does. Until that table
// exists, the dial is coarse: on by default (the blind-spot doctrine wins), with
// a single process-wide off switch (CONTENOX_TOOL_GUIDANCE=off) so the
// model-noise risk stays reachable. WrapWith already takes the thresholds as
// Options, so the day ModelProfile lands, this package changes at its edge, not
// its core.
//
// # What the eval harness must falsify
//
// Do NOT assume these
// counters help — measure it. The falsifiable claim is narrow and A/B-able on
// beam-over-LAN's recorded sessions: with guidance on, a unit's repeat-call and
// re-read rates fall and its landed-scope shrinks, WITHOUT a rise in
// malformed-tool-call rate (the weak-model noise failure). If the markers fire
// and behaviour does not move — or moves the wrong way on a weak model — the
// envelope is noise and the default flips. The harness measures the deltas; this
// package only supplies the signal and the switch.
package toolguidance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

// The three rules' default thresholds. They are HYPOTHESES, not constants
// (lesson 3): the numbers below are the maintainer's Stage-0 starting guesses,
// carried as Options so the eval harness and a future ModelProfile can move them
// without touching the mechanism.
const (
	// defaultRepeatThreshold fires the repeat-call marker on the Nth identical
	// (tool, args) call. 3 is "twice is coincidence, thrice is a pattern".
	defaultRepeatThreshold = 3
	// defaultScopeEvery emits one scope line every N tool calls. 15 is a
	// cadence: often enough to catch a widening blast radius, rare enough not to
	// ride every result.
	defaultScopeEvery = 15
	// defaultRevisitThreshold fires the re-read hint on the Nth read of one path.
	// 4 is "you have read this file three times; the fourth is a signal".
	defaultRevisitThreshold = 4
	// defaultMaxSessions bounds the per-session counter registry so a long-lived
	// serve process (one session key per turn) cannot leak unboundedly. Least-
	// recently-used sessions are evicted past this cap; eviction only loses stale
	// counters, never correctness of a live session.
	defaultMaxSessions = 512
)

// harnessPrefix is the fixed, greppable marker on every guidance line. It is the
// tool-hardening "clearly-marked envelope" made literal: a model (or a human
// grepping a transcript) can tell harness advice from tool content by this
// prefix alone, and a downstream renderer can strip or style it as a class.
const harnessPrefix = "[harness] "

// Options carries the three rule thresholds and the registry bound. It is the
// pre-ModelProfile dial: WrapWith takes it so per-model tuning is a construction
// argument, not a code change.
type Options struct {
	RepeatThreshold  int
	ScopeEvery       int
	RevisitThreshold int
	MaxSessions      int
}

// DefaultOptions returns the Stage-0 starting thresholds.
func DefaultOptions() Options {
	return Options{
		RepeatThreshold:  defaultRepeatThreshold,
		ScopeEvery:       defaultScopeEvery,
		RevisitThreshold: defaultRevisitThreshold,
		MaxSessions:      defaultMaxSessions,
	}
}

func (o Options) normalized() Options {
	if o.RepeatThreshold <= 0 {
		o.RepeatThreshold = defaultRepeatThreshold
	}
	if o.ScopeEvery <= 0 {
		o.ScopeEvery = defaultScopeEvery
	}
	if o.RevisitThreshold <= 0 {
		o.RevisitThreshold = defaultRevisitThreshold
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = defaultMaxSessions
	}
	return o
}

// sessionCtxKey binds an explicit session identity onto a context, mirroring
// missiontools.WithMissionID: an unexported key so no other package can collide,
// set once by a transport that knows the true session boundary.
type sessionCtxKey struct{}

// WithSession binds sessionID as the counter scope for every tool call on ctx.
// It is the PRECISE per-session hook: a transport that owns the real session
// boundary (an ACP session that spans many prompt turns) calls this once, and
// the counters then reset exactly on a new session — the "3rd identical list_dir
// this session" the blueprint names, verbatim. It is deliberately optional: when
// no transport binds it, the decorator falls back to the per-turn request id
// (see sessionKeyFromContext), so guidance still works without any wiring beyond
// the wrap — it simply scopes to the turn, which for an autonomous fleet unit is
// the whole agentic tool loop where intra-run repeats surface. An empty id is a
// no-op, so a caller that has no id to bind never marks a context with a blank
// scope.
func WithSession(ctx context.Context, sessionID string) context.Context {
	if strings.TrimSpace(sessionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// sessionKeyFromContext resolves the counter scope for a tool call, in priority
// order:
//
//  1. An explicit WithSession id — a transport that knows the true session
//     boundary (spans turns). This is the blueprint's literal "session".
//  2. The per-turn request id (libtracker.WithNewRequestID, set once per prompt).
//     For an autonomous unit, one turn is the whole tool loop, so this captures
//     intra-run repeats without any transport wiring. Its LIMIT, stated plainly:
//     across turns of the same chat ACP session the scope resets, so a
//     cross-turn re-read is not (yet) counted — that is what (1) upgrades, one
//     line, when a transport opts in.
//  3. A process-global bucket, last resort, so a call with neither id still gets
//     coherent (if coarse) counters rather than a per-call reset that would make
//     every rule dead.
func sessionKeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionCtxKey{}).(string); ok && v != "" {
		return "session:" + v
	}
	if v, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && v != "" {
		return "req:" + v
	}
	return "global"
}

// Enabled reports whether tool guidance is on. It is ON by default (the blind-
// spot doctrine): only an explicit off-ish value of CONTENOX_TOOL_GUIDANCE
// disables it. This is the single, coarse pre-ModelProfile switch — the escape
// hatch for the model-noise risk, reachable without a rebuild.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CONTENOX_TOOL_GUIDANCE"))) {
	case "off", "false", "0", "no", "disable", "disabled":
		return false
	default:
		return true
	}
}

// Wrap decorates any ToolsRepo with the default Stage-0 counters.
func Wrap(inner taskengine.ToolsRepo) taskengine.ToolsRepo {
	return WrapWith(inner, DefaultOptions())
}

// WrapWith decorates any ToolsRepo with counters tuned by opts. This is the ONE
// seam: it wraps the AGGREGATE tools repo, so every provider behind it
// (local_fs, local_shell, webtools, mission, MCP) is observed without any
// provider being touched — the same wrap-the-composed-repo shape the HITL gate
// already uses at enginesvc.buildTools. Guidance sits OUTSIDE the HITL wrapper so
// it observes only model-level calls and never the internal reads the HITL gate
// makes to build a diff.
func WrapWith(inner taskengine.ToolsRepo, opts Options) taskengine.ToolsRepo {
	opts = opts.normalized()
	return &decorator{
		inner: inner,
		opts:  opts,
		reg:   &registry{sessions: map[string]*sessionCounters{}, max: opts.MaxSessions},
	}
}

// WrapFromEnv wraps inner unless CONTENOX_TOOL_GUIDANCE is off, in which case it
// returns inner UNTOUCHED — when disabled, the decorator is not merely inert, it
// is absent, so an off switch costs exactly zero (no per-call branch, no
// allocation). This is the line a composition point calls.
func WrapFromEnv(inner taskengine.ToolsRepo) taskengine.ToolsRepo {
	if !Enabled() {
		return inner
	}
	return Wrap(inner)
}

// decorator is the ToolsRepo wrapper. It is transparent on every method except
// Exec, where — on a successful string result only — it may append up to two
// guidance lines.
type decorator struct {
	inner taskengine.ToolsRepo
	opts  Options
	reg   *registry
}

var _ taskengine.ToolsRepo = (*decorator)(nil)

// Exec runs the wrapped tool, then appends guidance. The order of the guards is
// the law:
//
//   - An error result short-circuits BEFORE any counting or appending — never
//     pile advice onto a failure (the tool-hardening error craft owns that
//     surface), and never let a failed call inflate the repeat/scope counters as
//     if it were navigation the model completed.
//   - A non-string (JSON/typed) result is returned byte-for-byte: appending text
//     would corrupt its shape, so guidance is COUNTED but not surfaced here — a
//     later string result in the same session carries the up-to-date totals.
//     This keeps the decorator transparent for structured tools (write_file,
//     mission_plan) while keeping their calls visible to scope.
func (d *decorator) Exec(ctx context.Context, startingTime time.Time, input any, debug bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := d.inner.Exec(ctx, startingTime, input, debug, call)
	if err != nil {
		return res, dt, err
	}
	if call == nil {
		return res, dt, err
	}

	// Count the call regardless of result shape (a JSON-returning write is still
	// navigation), then decide whether this particular result can carry the lines.
	lines := d.observe(ctx, input, call)

	s, ok := res.(string)
	if !ok || dt != taskengine.DataTypeString || len(lines) == 0 {
		return res, dt, err
	}
	return s + "\n" + strings.Join(lines, "\n"), dt, nil
}

// Supports delegates to the inner repo — the decorator changes results, never
// the tool surface.
func (d *decorator) Supports(ctx context.Context) ([]string, error) {
	return d.inner.Supports(ctx)
}

// GetSchemasForSupportedTools delegates to the inner repo.
func (d *decorator) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	return d.inner.GetSchemasForSupportedTools(ctx)
}

// GetToolsForToolsByName delegates to the inner repo.
func (d *decorator) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	return d.inner.GetToolsForToolsByName(ctx, name)
}

// observe updates the per-session counters for one call and returns at most two
// guidance lines, in priority order repeat > revisit > scope. It is where all
// three rules and the two-line cap live; it holds the session's lock for the
// whole update so the counters and the lines derived from them are consistent
// under concurrent tool calls.
func (d *decorator) observe(ctx context.Context, input any, call *taskengine.ToolsCall) []string {
	sc := d.reg.get(sessionKeyFromContext(ctx), d.opts.MaxSessions)
	leaf, full := toolNames(call)
	fp := fingerprint(full, input, call)
	path := extractPath(input, call)

	sc.mu.Lock()
	defer sc.mu.Unlock()

	var out []string

	// Rule 1 — repeat-call marker. Identical (tool, args-fingerprint) at or past
	// the threshold; the ordinal updates on every further repeat.
	sc.repeats[fp]++
	if n := sc.repeats[fp]; n >= d.opts.RepeatThreshold {
		out = append(out, fmt.Sprintf("%s%s identical %s call this session.", harnessPrefix, ordinal(n), leaf))
	}

	// Rule 3 — revisit hint. Only read-like tools on a real path count toward a
	// re-READ, so the word "read" in the line stays truthful.
	if path != "" && isReadLike(leaf) {
		sc.reads[path]++
		if n := sc.reads[path]; n >= d.opts.RevisitThreshold {
			out = append(out, fmt.Sprintf("%s%s read of %s this session.", harnessPrefix, ordinal(n), path))
		}
	}

	// Rule 2 — scope line, every N calls. Accumulate the touched-path set first
	// (a dir tool's path is a directory; anything else contributes a file and its
	// parent directory), then emit the cadence line.
	sc.calls++
	if path != "" {
		if isDirTool(leaf) {
			sc.dirs[path] = struct{}{}
		} else {
			sc.files[path] = struct{}{}
			sc.dirs[dirOf(path)] = struct{}{}
		}
	}
	if sc.calls%d.opts.ScopeEvery == 0 {
		out = append(out, fmt.Sprintf("%sscope so far: %d files across %d directories.", harnessPrefix, len(sc.files), len(sc.dirs)))
	}

	// The two-line cap — the envelope stays terse. When all three fire on one
	// call, the most specific advice (a repeat, a re-read) wins over the periodic
	// scope line.
	if len(out) > 2 {
		out = out[:2]
	}
	return out
}

// registry holds per-session counters, keyed by the resolved session key. It is
// bounded: past max sessions the least-recently-used entry is evicted, which
// only ever discards stale counters (a session that has gone quiet), never a
// live one's state mid-use.
type registry struct {
	mu       sync.Mutex
	sessions map[string]*sessionCounters
	seq      uint64
	max      int
}

func (r *registry) get(key string, max int) *sessionCounters {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	if sc, ok := r.sessions[key]; ok {
		sc.lastSeq = r.seq
		return sc
	}
	if max > 0 && len(r.sessions) >= max {
		r.evictLRULocked()
	}
	sc := newSessionCounters(r.seq)
	r.sessions[key] = sc
	return sc
}

// evictLRULocked drops the least-recently-touched session. Called under r.mu.
// A linear scan is fine: eviction is rare (only at the cap) and max is small.
func (r *registry) evictLRULocked() {
	var oldestKey string
	var oldestSeq uint64
	first := true
	for k, sc := range r.sessions {
		if first || sc.lastSeq < oldestSeq {
			oldestKey, oldestSeq, first = k, sc.lastSeq, false
		}
	}
	if !first {
		delete(r.sessions, oldestKey)
	}
}

// sessionCounters is one session's deterministic state. Each field is guarded by
// mu; a session is independent of every other, so two concurrent sessions never
// contend, and two concurrent calls WITHIN a session serialize on this lock.
type sessionCounters struct {
	mu      sync.Mutex
	lastSeq uint64
	calls   int
	repeats map[string]int      // args-fingerprint -> count (rule 1)
	reads   map[string]int      // path -> read-like count (rule 3)
	files   map[string]struct{} // distinct file paths touched (rule 2)
	dirs    map[string]struct{} // distinct directories touched (rule 2)
}

func newSessionCounters(seq uint64) *sessionCounters {
	return &sessionCounters{
		lastSeq: seq,
		repeats: map[string]int{},
		reads:   map[string]int{},
		files:   map[string]struct{}{},
		dirs:    map[string]struct{}{},
	}
}

// toolNames returns the terse leaf name (for the human-facing line) and the
// fully-qualified provider.leaf name (for the fingerprint, so the same leaf on
// two providers never collides).
func toolNames(call *taskengine.ToolsCall) (leaf, full string) {
	leaf = call.ToolName
	if leaf == "" {
		leaf = call.Name
	}
	switch {
	case call.Name != "" && call.ToolName != "":
		full = call.Name + "." + call.ToolName
	case call.ToolName != "":
		full = call.ToolName
	default:
		full = call.Name
	}
	return leaf, full
}

// fingerprint is a stable hash of (fully-qualified tool, canonicalized args).
// Canonicalization is the whole point of "identical": two calls are the same
// call iff their tool and their argument set match regardless of map ordering.
//
// Both argument shapes a tool call arrives in are merged (see argEngine notes in
// missiontools): the deterministic `tools` task's map[string]string Args, and the
// model-driven map[string]any input, with the model shape winning on a clash —
// the same precedence the providers themselves use. Each value is JSON-encoded so
// nested objects/arrays canonicalize deterministically (encoding/json sorts
// object keys), and keys are sorted before hashing.
//
// Policy/harness-injected keys (leading underscore, e.g. _allowed_dir) are
// EXCLUDED: they are not the model's intent, they are the composition point's,
// and folding them in would split the fingerprint of two identical model calls
// that happened to run under different policy — defeating the rule.
func fingerprint(full string, input any, call *taskengine.ToolsCall) string {
	fields := map[string]string{}
	if call != nil && call.Args != nil {
		for k, v := range call.Args {
			if strings.HasPrefix(k, "_") {
				continue
			}
			fields[k] = v
		}
	}
	if m, ok := input.(map[string]any); ok {
		for k, v := range m {
			if strings.HasPrefix(k, "_") {
				continue
			}
			fields[k] = canonicalValue(v)
		}
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(full))
	h.Write([]byte{0})
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(fields[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func canonicalValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// pathArgKeys is the path-extraction heuristic: the argument names across the
// local providers (fs.go, localexec.go) and MCP tools that carry a filesystem
// path. Its LIMITS, documented because lesson 3 demands honesty about the signal:
//
//   - It reads DECLARED path args only. A tool whose path is buried in an
//     arbitrary shell `command`, a glob `pattern`, or a URL is invisible to
//     scope — a known blind spot, not a bug to paper over.
//   - It takes the FIRST matching key, so a two-path tool (a copy with src+dst)
//     contributes only one path. The common case (one path per call) is exact;
//     the rare case under-counts rather than guessing.
//   - It does not stat the path, so "files vs directories" is inferred from the
//     tool name (isDirTool), not the filesystem — a rough split, good enough for
//     a scope signal, never presented as ground truth.
var pathArgKeys = []string{"path", "file", "file_path", "filepath", "filename", "dir", "dir_path", "directory", "target"}

func extractPath(input any, call *taskengine.ToolsCall) string {
	if m, ok := input.(map[string]any); ok {
		for _, k := range pathArgKeys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok {
					if t := strings.TrimSpace(s); t != "" {
						return t
					}
				}
			}
		}
	}
	if call != nil && call.Args != nil {
		for _, k := range pathArgKeys {
			if t := strings.TrimSpace(call.Args[k]); t != "" {
				return t
			}
		}
	}
	return ""
}

func dirOf(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}

// isReadLike reports whether a leaf tool name is a file READ — so the revisit
// hint's "Nth read of X" only fires on tools that actually read. Substring
// "read" catches read_file / read_file_range; the small set covers common
// aliases without pulling in list/grep (which are navigation, but not re-reads).
func isReadLike(leaf string) bool {
	l := strings.ToLower(leaf)
	if strings.Contains(l, "read") {
		return true
	}
	switch l {
	case "cat", "view", "open", "stat_file":
		return true
	}
	return false
}

// isDirTool reports whether a leaf tool name operates on a directory, so the
// scope line credits its path as a directory rather than a file.
func isDirTool(leaf string) bool {
	l := strings.ToLower(leaf)
	return strings.Contains(l, "dir") || strings.Contains(l, "list") || l == "ls"
}

// ordinal renders 1->1st, 2->2nd, 3->3rd, 4->4th, 11->11th, 21->21st, ... The
// guidance lines read as prose ("3rd identical list_dir call"), so the ordinal
// must too.
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}
