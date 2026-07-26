package localtools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

// ---------------------------------------------------------------------------
// Argument coercion
//
// Small local models (qwen3-4b and friends) routinely emit JSON-encoded
// scalars as strings: {"start_line": "5"}, {"recursive": "true"}. The strict
// type assertions this package used previously caused those calls to silently
// fall through to defaults — a ranged read would become a full read, and the
// model got no signal that its argument had been dropped. Coercion here is
// deliberately generous; rejectUnknownArgs still guards the argument *names*.
// ---------------------------------------------------------------------------

// argString returns a string value for key, accepting a real string or any
// scalar that has an unambiguous string form.
func argString(args map[string]any, key string) (string, bool) {
	x, exists := args[key]
	if !exists || x == nil {
		return "", false
	}
	switch v := x.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return "", false
	}
}

// argBool returns the boolean value for key. Accepts real JSON booleans, the
// strings "true"/"false"/"1"/"0"/"yes"/"no" in any case, and numeric 0/1.
func argBool(args map[string]any, key string) (v bool, ok bool) {
	x, exists := args[key]
	if !exists || x == nil {
		return false, false
	}
	switch b := x.(type) {
	case bool:
		return b, true
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes", "y", "on":
			return true, true
		case "false", "0", "no", "n", "off":
			return false, true
		}
		return false, false
	case json.Number:
		if n, err := b.Float64(); err == nil {
			return n != 0, true
		}
		return false, false
	case float64:
		return b != 0, true
	case int:
		return b != 0, true
	case int64:
		return b != 0, true
	default:
		return false, false
	}
}

// argFloat returns a float64 for key. Accepts JSON numbers (which decode as
// float64), json.Number, Go integer types, and numeric strings.
func argFloat(args map[string]any, key string) (v float64, ok bool) {
	x, exists := args[key]
	if !exists || x == nil {
		return 0, false
	}
	switch n := x.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// argInt is argFloat narrowed to int, for line numbers and offsets.
func argInt(args map[string]any, key string) (int, bool) {
	f, ok := argFloat(args, key)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// ---------------------------------------------------------------------------
// Policy keys
//
// All keys live under tools_policies.local_fs and arrive as strings via
// taskengine.ToolsArgsFromContext.
// ---------------------------------------------------------------------------

const (
	// defaultMaxOutputBytes is the ceiling on any single tool result.
	//
	// This used to be 512 KiB, which is roughly 130k tokens of text — a cap
	// calibrated to what a filesystem can hand back rather than to what a
	// model can accept. It could not fire before the context window did, so
	// oversized results sailed through this check and failed later at model
	// resolution, wedging the session with an unshrinkable history entry.
	// 32 KiB is ~8k tokens: large enough for real listings, small enough that
	// a single tool result cannot dominate a small model's context.
	//
	// Prefer setting _model_context_tokens and letting the budget be derived
	// (see maxOutputBytesFromPolicy) over overriding this directly.
	defaultMaxOutputBytes = 32 * 1024

	// defaultMaxReadBytes caps a whole-file read. Files above this must be
	// paged with start_line/end_line.
	defaultMaxReadBytes = 1024 * 1024

	// contextBytesPerToken is a deliberately conservative bytes-per-token
	// estimate for deriving an output budget from a model's context window.
	// Real English prose runs ~4 B/token; source code and path listings run
	// closer to 3. Under-estimating here means a smaller budget, which is the
	// safe direction.
	contextBytesPerToken = 3.0

	// contextBudgetFraction is the share of a model's total context window
	// that any single tool result may consume. One result taking a quarter of
	// the window still leaves room for the system prompt, tool schemas,
	// conversation history, and the model's own reply.
	contextBudgetFraction = 0.25
)

func (h *LocalFSTools) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, h.name)
}

func (h *LocalFSTools) policyString(ctx context.Context, key string) (string, bool) {
	args := h.policyArgs(ctx)
	if args == nil {
		return "", false
	}
	v, ok := args[key]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// policyInt reads an integer policy key, clamping to [min, max] and falling
// back to def when absent or unparseable.
func (h *LocalFSTools) policyInt(ctx context.Context, key string, def, min, max int) int {
	s, ok := h.policyString(ctx, key)
	if !ok || s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func (h *LocalFSTools) policyBool(ctx context.Context, key string, def bool) bool {
	s, ok := h.policyString(ctx, key)
	if !ok || s == "" {
		return def
	}
	switch strings.ToLower(s) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

// maxListDepthFromPolicy caps recursion depth for list_dir(recursive).
// Policy key: _max_list_depth — default 6, hard ceiling 32.
func (h *LocalFSTools) maxListDepthFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_list_depth", 6, 1, 32)
}

// maxListEntriesScannedFromPolicy bounds how many filesystem entries a single
// recursive list_dir will visit, independent of how many it returns. Without
// this, paging through a pathological tree with a large offset re-walks the
// whole tree on every call.
// Policy key: _max_list_scan — default 100000.
func (h *LocalFSTools) maxListEntriesScannedFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_list_scan", 100000, 100, 10000000)
}

// maxGrepMatchesFromPolicy caps the number of grep matches returned.
// Policy key: _max_grep_matches — default 500.
//
// Lowered from 5000: at the previous value, hitting the cap was an error that
// discarded every match already found. Matches are now returned with a
// truncation notice, so the cap is about result size rather than aborting.
func (h *LocalFSTools) maxGrepMatchesFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_grep_matches", 500, 1, 500000)
}

// maxFindResultsFromPolicy caps find_files results.
// Policy key: _max_find_results — default 200.
func (h *LocalFSTools) maxFindResultsFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_find_results", 200, 1, 5000)
}

// maxFindDepthFromPolicy bounds how deep find_files descends.
// Policy key: _max_find_depth — default 24.
func (h *LocalFSTools) maxFindDepthFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_find_depth", 24, 1, 128)
}

// verboseToolDescriptions reports whether the long-form tool descriptions
// should be emitted. Off by default: the terse schema costs a few hundred
// tokens per turn instead of a few thousand, and every behaviour the long form
// pre-teaches is re-taught by the error messages at the moment it matters,
// with concrete values filled in.
// Policy key: _verbose_tool_descriptions — default false.
func (h *LocalFSTools) verboseToolDescriptions(ctx context.Context) bool {
	return h.policyBool(ctx, "_verbose_tool_descriptions", false)
}

// useGitignoreFromPolicy reports whether .gitignore should be consulted when
// filtering directory listings and file searches.
// Policy key: _use_gitignore — default true.
func (h *LocalFSTools) useGitignoreFromPolicy(ctx context.Context) bool {
	return h.policyBool(ctx, "_use_gitignore", true)
}

// maxOutputBytesFromPolicy returns the ceiling on a single tool result.
//
// Resolution order:
//  1. _max_output_bytes, when set (non-positive means unlimited)
//  2. derived from _model_context_tokens, when set
//  3. defaultMaxOutputBytes
//
// The derived path is the one to prefer: whoever assembles the chain knows
// which model was resolved, and injecting _model_context_tokens lets the cap
// track the actual window instead of a constant chosen years earlier.
func (h *LocalFSTools) maxOutputBytesFromPolicy(ctx context.Context) (limit int64, unlimited bool) {
	if s, ok := h.policyString(ctx, "_max_output_bytes"); ok && s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return defaultMaxOutputBytes, false
		}
		if n <= 0 {
			return 0, true
		}
		return n, false
	}
	if s, ok := h.policyString(ctx, "_model_context_tokens"); ok && s != "" {
		if tokens, err := strconv.ParseInt(s, 10, 64); err == nil && tokens > 0 {
			derived := int64(float64(tokens) * contextBytesPerToken * contextBudgetFraction)
			// Floor: a budget so small that no useful result fits is worse
			// than a slightly oversized one, because the model cannot make
			// progress at all.
			if derived < 4096 {
				derived = 4096
			}
			return derived, false
		}
	}
	return defaultMaxOutputBytes, false
}

// maxReadBytesFromPolicy returns the max bytes for a full-file read.
// Policy key: _max_read_bytes — default 1 MiB. Non-positive means unlimited.
func (h *LocalFSTools) maxReadBytesFromPolicy(ctx context.Context) (limit int64, unlimited bool) {
	s, ok := h.policyString(ctx, "_max_read_bytes")
	if !ok || s == "" {
		return defaultMaxReadBytes, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultMaxReadBytes, false
	}
	if n <= 0 {
		return 0, true
	}
	return n, false
}

// defaultSkipDirNames is the fallback set of directory basenames omitted from
// listings when .gitignore is unavailable or disabled.
//
// Basename matching alone is a losing game — it can only ever enumerate the
// noise directories someone thought of in advance, and misses every
// project-specific one (build output, scratch dirs, vendored comparison
// checkouts, local model caches). .gitignore is the real filter; this list is
// the backstop for trees that aren't git repositories.
var defaultSkipDirNames = []string{
	".git", ".hg", ".svn",
	"node_modules", "bower_components", "Pods",
	".venv", "venv", "env", "__pycache__",
	".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox",
	".next", ".nuxt", ".turbo", ".parcel-cache",
	"dist", "build", "out", "target", "coverage",
	".cache", ".gradle", ".terraform",
	"vendor",
	".idea", ".vscode",
}

// skipDirNamesFromPolicy returns the set of directory basenames that listings
// silently omit.
// Policy key: _skip_dir_names — comma-separated basenames. Absent uses the
// default set; an explicit empty string disables basename filtering entirely.
func (h *LocalFSTools) skipDirNamesFromPolicy(ctx context.Context) map[string]bool {
	raw, keyPresent := h.policyString(ctx, "_skip_dir_names")
	if !keyPresent {
		return skipDirNameSet(defaultSkipDirNames)
	}
	if raw == "" {
		return nil // disabled: show everything
	}
	var names []string
	for _, s := range strings.Split(raw, ",") {
		if n := strings.TrimSpace(s); n != "" {
			names = append(names, n)
		}
	}
	return skipDirNameSet(names)
}

func skipDirNameSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// listExtensionsFromPolicy returns the set of lower-cased file extensions that
// listings include. Absent or empty means all files.
// Policy key: _list_extensions — comma-separated, leading dot optional.
func (h *LocalFSTools) listExtensionsFromPolicy(ctx context.Context) map[string]bool {
	raw, ok := h.policyString(ctx, "_list_extensions")
	if !ok || raw == "" {
		return nil
	}
	m := make(map[string]bool)
	for _, s := range strings.Split(raw, ",") {
		ext := strings.ToLower(strings.TrimSpace(s))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		m[ext] = true
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// deniedSubstringsFromPolicy returns the configured denied path substrings.
// Policy key: _denied_path_substrings — comma-separated.
func (h *LocalFSTools) deniedSubstringsFromPolicy(ctx context.Context) []string {
	raw, ok := h.policyString(ctx, "_denied_path_substrings")
	if !ok || raw == "" {
		return nil
	}
	var out []string
	for _, pat := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(pat); p != "" {
			out = append(out, strings.ReplaceAll(p, "\\", "/"))
		}
	}
	return out
}
