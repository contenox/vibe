// Package toolguidance appends short orientation lines to a tool's textual
// result: a repeat-call marker, a re-read hint, and a periodic scope
// summary, derived from per-session counters the harness maintains. It only
// appends — never changes a result's shape, never fails or blocks a call —
// and caps output at two lines per call. Toggle with
// CONTENOX_TOOL_GUIDANCE=off.
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

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

// Default thresholds for the three rules, tunable via Options.
const (
	// defaultRepeatThreshold fires the repeat-call marker on the Nth
	// identical (tool, args) call.
	defaultRepeatThreshold = 3
	// defaultScopeEvery emits one scope line every N tool calls.
	defaultScopeEvery = 15
	// defaultRevisitThreshold fires the re-read hint on the Nth read of one path.
	defaultRevisitThreshold = 4
	// defaultMaxSessions bounds the per-session counter registry;
	// least-recently-used sessions are evicted past this cap.
	defaultMaxSessions = 512
)

// harnessPrefix marks every guidance line so it is distinguishable from tool
// content by prefix alone.
const harnessPrefix = "[harness] "

// Options carries the three rule thresholds and the session-registry bound.
type Options struct {
	RepeatThreshold  int
	ScopeEvery       int
	RevisitThreshold int
	MaxSessions      int
}

// DefaultOptions returns the default thresholds.
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

// sessionCtxKey is an unexported context key for the bound session id.
type sessionCtxKey struct{}

// WithSession binds sessionID as the counter scope for every tool call on
// ctx. Optional: if unset, the decorator falls back to the per-turn request
// id (see sessionKeyFromContext). An empty sessionID is a no-op.
func WithSession(ctx context.Context, sessionID string) context.Context {
	if strings.TrimSpace(sessionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// sessionKeyFromContext resolves the counter scope for a tool call, in
// priority order: an explicit WithSession id, then the per-turn request id
// (libtracker.WithNewRequestID), then a process-global fallback bucket.
func sessionKeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionCtxKey{}).(string); ok && v != "" {
		return "session:" + v
	}
	if v, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && v != "" {
		return "req:" + v
	}
	return "global"
}

// Enabled reports whether tool guidance is on. It defaults to true; only an
// explicit off-ish CONTENOX_TOOL_GUIDANCE value disables it.
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

// WrapWith decorates inner with counters tuned by opts. It wraps the
// aggregate ToolsRepo, so every provider behind it is observed uniformly,
// and it should sit outside any HITL wrapper so it never sees the HITL
// gate's internal reads.
func WrapWith(inner taskengine.ToolsRepo, opts Options) taskengine.ToolsRepo {
	opts = opts.normalized()
	return &decorator{
		inner: inner,
		opts:  opts,
		reg:   &registry{sessions: map[string]*sessionCounters{}, max: opts.MaxSessions},
	}
}

// WrapFromEnv wraps inner unless CONTENOX_TOOL_GUIDANCE is off, in which case
// it returns inner unchanged so a disabled guidance layer costs nothing.
func WrapFromEnv(inner taskengine.ToolsRepo) taskengine.ToolsRepo {
	if !Enabled() {
		return inner
	}
	return Wrap(inner)
}

// decorator is the ToolsRepo wrapper. It is transparent on every method
// except Exec, where a successful string result may gain up to two guidance
// lines.
type decorator struct {
	inner taskengine.ToolsRepo
	opts  Options
	reg   *registry
}

var _ taskengine.ToolsRepo = (*decorator)(nil)

// Exec runs the wrapped tool, then appends guidance. An error result is
// returned untouched before any counting or appending. A non-string result
// is counted but not appended to, unless it implements
// AppendGuidance(string) any.
func (d *decorator) Exec(ctx context.Context, startingTime time.Time, input any, debug bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := d.inner.Exec(ctx, startingTime, input, debug, call)
	if err != nil {
		return res, dt, err
	}
	if call == nil {
		return res, dt, err
	}

	// Count regardless of result shape; only a string result can carry the lines.
	lines := d.observe(ctx, input, call)

	if len(lines) == 0 || dt != taskengine.DataTypeString {
		return res, dt, err
	}
	suffix := "\n" + strings.Join(lines, "\n")
	if s, ok := res.(string); ok {
		return s + suffix, dt, nil
	}
	// A typed result that renders as text can still carry the lines via this
	// optional interface, asserted structurally so this package depends on
	// no toolset.
	if carrier, ok := res.(interface{ AppendGuidance(string) any }); ok {
		return carrier.AppendGuidance(suffix), dt, nil
	}
	return res, dt, err
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

// observe updates the per-session counters for one call and returns at most
// two guidance lines, priority repeat > revisit > scope. Holds the
// session's lock for the whole update.
func (d *decorator) observe(ctx context.Context, input any, call *taskengine.ToolsCall) []string {
	sc := d.reg.get(sessionKeyFromContext(ctx), d.opts.MaxSessions)
	leaf, full := toolNames(call)
	fp := fingerprint(full, input, call)
	path := extractPath(input, call)

	sc.mu.Lock()
	defer sc.mu.Unlock()

	var out []string

	// Rule 1: repeat-call marker.
	sc.repeats[fp]++
	if n := sc.repeats[fp]; n >= d.opts.RepeatThreshold {
		out = append(out, fmt.Sprintf("%s%s identical %s call this session.", harnessPrefix, ordinal(n), leaf))
	}

	// Rule 3: revisit hint, read-like tools only.
	if path != "" && isReadLike(leaf) {
		sc.reads[path]++
		if n := sc.reads[path]; n >= d.opts.RevisitThreshold {
			out = append(out, fmt.Sprintf("%s%s read of %s this session.", harnessPrefix, ordinal(n), path))
		}
	}

	// Rule 2: scope line every N calls; a dir tool's path counts as a
	// directory, others as a file plus its parent directory.
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

	// Two-line cap: the most specific advice wins over the periodic scope line.
	if len(out) > 2 {
		out = out[:2]
	}
	return out
}

// registry holds per-session counters, bounded to max sessions with
// least-recently-used eviction.
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

// evictLRULocked drops the least-recently-touched session. Caller holds r.mu.
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

// sessionCounters is one session's state, guarded by mu.
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

// toolNames returns the leaf name (for display) and the fully-qualified
// provider.leaf name (for the fingerprint).
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

// fingerprint is a stable hash of (fully-qualified tool, canonicalized
// args): two calls are identical iff tool and argument set match regardless
// of map ordering. call.Args and the model's input map are merged, model
// values winning on a clash; underscore-prefixed keys (harness-injected, not
// model intent) are excluded.
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

// pathArgKeys are the argument names that carry a filesystem path across the
// local providers and MCP tools. Limits: only declared path args are seen; a
// multi-path tool contributes only the first match; file-vs-directory is
// inferred from the tool name, not the filesystem.
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

// isReadLike reports whether a leaf tool name is a file read.
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

// isDirTool reports whether a leaf tool name operates on a directory.
func isDirTool(leaf string) bool {
	l := strings.ToLower(leaf)
	return strings.Contains(l, "dir") || strings.Contains(l, "list") || l == "ls"
}

// ordinal renders 1->1st, 2->2nd, 3->3rd, 4->4th, 11->11th, 21->21st, ...
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
