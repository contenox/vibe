package localtools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

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

func argInt(args map[string]any, key string) (int, bool) {
	f, ok := argFloat(args, key)
	if !ok {
		return 0, false
	}
	return int(f), true
}

const (
	defaultMaxOutputBytes = 32 * 1024

	defaultMaxReadBytes = 1024 * 1024

	defaultMaxAudioBytes = 14 * 1024 * 1024

	contextBytesPerToken = 3.0

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

func (h *LocalFSTools) maxListDepthFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_list_depth", 6, 1, 32)
}

func (h *LocalFSTools) maxListEntriesScannedFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_list_scan", 100000, 100, 10000000)
}

func (h *LocalFSTools) maxGrepMatchesFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_grep_matches", 500, 1, 500000)
}

func (h *LocalFSTools) maxFindResultsFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_find_results", 200, 1, 5000)
}

func (h *LocalFSTools) maxFindDepthFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_find_depth", 24, 1, 128)
}

func (h *LocalFSTools) verboseToolDescriptions(ctx context.Context) bool {
	return h.policyBool(ctx, "_verbose_tool_descriptions", false)
}

func (h *LocalFSTools) useGitignoreFromPolicy(ctx context.Context) bool {
	return h.policyBool(ctx, "_use_gitignore", true)
}

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
			if derived < 4096 {
				derived = 4096
			}
			return derived, false
		}
	}
	return defaultMaxOutputBytes, false
}

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

func (h *LocalFSTools) maxAudioBytesFromPolicy(ctx context.Context) (limit int64, unlimited bool) {
	s, ok := h.policyString(ctx, "_max_audio_bytes")
	if !ok || s == "" {
		return defaultMaxAudioBytes, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return defaultMaxAudioBytes, false
	}
	if n <= 0 {
		return 0, true
	}
	return n, false
}

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
