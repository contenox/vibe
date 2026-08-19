package localtools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
)

// LocalFSBrowseToolsName is the registered toolset name an allowlist addresses;
// the native- scope is a namespace, so a declared MCP source cannot mint the
// same key.
const LocalFSBrowseToolsName = "native-fs-browse"

// LocalFSBrowseTools walks the host filesystem directly rather than through
// FileIO, so it is only ever registered on a profile that owns the machine.
type LocalFSBrowseTools struct {
	allowedDir  string
	name        string
	cwdResolver func(context.Context) string
}

func NewLocalFSBrowseTools(allowedDir string, cwdResolver func(context.Context) string) taskengine.ToolsRepo {
	return newLocalFSBrowseTools(allowedDir, LocalFSBrowseToolsName, cwdResolver)
}

func newLocalFSBrowseTools(allowedDir, name string, cwdResolver func(context.Context) string) *LocalFSBrowseTools {
	if name == "" {
		name = LocalFSBrowseToolsName
	}
	cleaned := allowedDir
	if cleaned != "" {
		cleaned = filepath.Clean(cleaned)
	}
	return &LocalFSBrowseTools{allowedDir: cleaned, name: name, cwdResolver: cwdResolver}
}

// BrowseFilter returns the listing predicate list_dir and find_files apply under default policy (the workspace .gitignore at absRoot plus default skip-directory basenames), reporting true for entries a listing omits; it is a noise filter, never access control.
func BrowseFilter(absRoot string) func(rel, base string, isDir bool) bool {
	f := entryFilter{
		skipDirs: skipDirNameSet(defaultSkipDirNames),
		ignore:   gitignoreFor(absRoot),
	}
	return f.skip
}

// Exec wraps execDispatch and stamps every returned error fatal-vs-recoverable.
func (h *LocalFSBrowseTools) Exec(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := h.execDispatch(ctx, startTime, input, debug, toolsCall)
	return res, dt, markSeverity(err)
}

func (h *LocalFSBrowseTools) execDispatch(ctx context.Context, _ time.Time, input any, _ bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if toolsCall == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: tools required", h.name)
	}

	args, ok := input.(map[string]any)
	if !ok {
		// Fall back to ToolsCall.Args when the chain input isn't an args map.
		if len(toolsCall.Args) > 0 {
			args = make(map[string]any, len(toolsCall.Args))
			for k, v := range toolsCall.Args {
				args[k] = v
			}
		} else {
			return nil, taskengine.DataTypeAny, fmt.Errorf("%s: input must be a map (or provide tools.args)", h.name)
		}
	}

	toolName := toolsCall.ToolName
	if toolName == "" {
		toolName = toolsCall.Name
	}

	switch toolName {
	case "list_dir":
		if err := rejectUnknownArgs(h.name+".list_dir", args, "path", "recursive", "max_depth", "offset"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.listDir(ctx, args)
	case "grep":
		if err := rejectUnknownArgs(h.name+".grep", args, "path", "pattern", "regex", "start_line", "end_line"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.grep(ctx, args)
	case "find_files":
		if err := rejectUnknownArgs(h.name+".find_files", args, "pattern", "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.findFiles(ctx, args)
	case "count_stats":
		if err := rejectUnknownArgs(h.name+".count_stats", args, "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.countStats(ctx, args)
	case "stat_file":
		if err := rejectUnknownArgs(h.name+".stat_file", args, "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.statFile(ctx, args)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("%s: unknown tool %s", h.name, toolName)
	}
}

func (h *LocalFSBrowseTools) policyKey(key string) string {
	return "tools_policies." + h.name + "." + key
}

func (h *LocalFSBrowseTools) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, h.name)
}

func (h *LocalFSBrowseTools) policyString(ctx context.Context, key string) (string, bool) {
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

func (h *LocalFSBrowseTools) policyInt(ctx context.Context, key string, def, min, max int) int {
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

func (h *LocalFSBrowseTools) policyBool(ctx context.Context, key string, def bool) bool {
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

func (h *LocalFSBrowseTools) maxListDepthFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_list_depth", 6, 1, 32)
}

func (h *LocalFSBrowseTools) maxListEntriesScannedFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_list_scan", 100000, 100, 10000000)
}

func (h *LocalFSBrowseTools) maxGrepMatchesFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_grep_matches", 500, 1, 500000)
}

func (h *LocalFSBrowseTools) maxFindResultsFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_find_results", 200, 1, 5000)
}

func (h *LocalFSBrowseTools) maxFindDepthFromPolicy(ctx context.Context) int {
	return h.policyInt(ctx, "_max_find_depth", 24, 1, 128)
}

func (h *LocalFSBrowseTools) verboseToolDescriptions(ctx context.Context) bool {
	return h.policyBool(ctx, "_verbose_tool_descriptions", false)
}

func (h *LocalFSBrowseTools) useGitignoreFromPolicy(ctx context.Context) bool {
	return h.policyBool(ctx, "_use_gitignore", true)
}

func (h *LocalFSBrowseTools) maxOutputBytesFromPolicy(ctx context.Context) (limit int64, unlimited bool) {
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

func (h *LocalFSBrowseTools) maxReadBytesFromPolicy(ctx context.Context) (limit int64, unlimited bool) {
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

func (h *LocalFSBrowseTools) skipDirNamesFromPolicy(ctx context.Context) map[string]bool {
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

func (h *LocalFSBrowseTools) listExtensionsFromPolicy(ctx context.Context) map[string]bool {
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

func (h *LocalFSBrowseTools) deniedSubstringsFromPolicy(ctx context.Context) []string {
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

func (h *LocalFSBrowseTools) baseDir(ctx context.Context) (string, error) {
	if args := h.policyArgs(ctx); len(args) > 0 {
		if policyDir := strings.TrimSpace(args["_allowed_dir"]); policyDir != "" {
			cleaned := filepath.Clean(policyDir)
			if filepath.IsAbs(cleaned) {
				return cleaned, nil
			}
			if cwd := h.resolveCwd(ctx); cwd != "" {
				return filepath.Clean(filepath.Join(cwd, cleaned)), nil
			}
			// Falling through to OS path resolution would silently scope calls to
			// this process's cwd.
			return "", fmt.Errorf(
				"%s: %s %q is relative but no session workspace root could be resolved to anchor it "+
					"(this run has no live cwd resolver and its checkpoint carries no restored workspace root); "+
					"set _allowed_dir to an absolute path, or resume from a process that can restore the session's workspace root",
				h.name, h.policyKey("_allowed_dir"), policyDir)
		}
	}
	base := h.allowedDir
	if base == "" {
		base = h.resolveCwd(ctx)
	}
	if base == "" {
		return "", fmt.Errorf("%s: no allowed directory configured", h.name)
	}
	return base, nil
}

func (h *LocalFSBrowseTools) resolveCwd(ctx context.Context) string {
	if root := vfs.SessionCwdFromContext(ctx); root != "" {
		return filepath.Clean(root)
	}
	if h.cwdResolver != nil {
		if r := h.cwdResolver(ctx); r != "" {
			return filepath.Clean(r)
		}
	}
	return ""
}

func (h *LocalFSBrowseTools) absAllowedDir(ctx context.Context) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.ResolveRoot(base)
	if err != nil {
		return "", fmt.Errorf("%s: invalid allowed dir: %w", h.name, err)
	}
	return resolved, nil
}

func (h *LocalFSBrowseTools) checkPath(ctx context.Context, path string) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.Contain(base, path)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			return "", fmt.Errorf("%s: path %s escapes allowed directory %s", h.name, path, base)
		}
		return "", fmt.Errorf("%s: %w", h.name, err)
	}
	return resolved, nil
}

func (h *LocalFSBrowseTools) displayPath(ctx context.Context, absPath string) string {
	base, err := h.absAllowedDir(ctx)
	if err != nil {
		return absPath
	}
	rel, err := filepath.Rel(base, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return absPath
	}
	return filepath.ToSlash(rel)
}

func (h *LocalFSBrowseTools) checkDeniedSubstrings(ctx context.Context, absPath string) error {
	subs := h.deniedSubstringsFromPolicy(ctx)
	if len(subs) == 0 {
		return nil
	}
	rel := h.displayPath(ctx, absPath)
	for _, p := range subs {
		if strings.Contains(rel, p) {
			return fmt.Errorf("%s: path %q matches denied substring %q (%s)", h.name, rel, p, h.policyKey("_denied_path_substrings"))
		}
	}
	return nil
}

func (h *LocalFSBrowseTools) checkFileSizeLimit(ctx context.Context, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("%s: stat: %w", h.name, err)
	}
	if info.IsDir() {
		return nil
	}
	limit, unlimited := h.maxReadBytesFromPolicy(ctx)
	if unlimited {
		return nil
	}
	if info.Size() > limit {
		return recoverablef("%s: file is %d bytes (max %d); narrow the path or raise %s", h.name, info.Size(), limit, h.policyKey("_max_read_bytes"))
	}
	return nil
}

// refuseBinary sniffs the first bytes off disk rather than loading the file,
// so a multi-gigabyte binary costs one short read.
func (h *LocalFSBrowseTools) refuseBinary(tool, displayPath, absPath string) error {
	binary, err := sniffBinaryFile(absPath)
	if err != nil || !binary {
		return nil
	}
	detail := "binary file"
	if info, statErr := os.Stat(absPath); statErr == nil {
		detail = fmt.Sprintf("binary file (%s)", fileSizeAndExecFlag(info))
	}
	return recoverablef("%s: %s: refusing to read %s: %s. Use shell tools for binaries.", h.name, tool, displayPath, detail)
}

func (h *LocalFSBrowseTools) checkToolOutputLimit(ctx context.Context, tool string, payload string) error {
	limit, unlimited := h.maxOutputBytesFromPolicy(ctx)
	if unlimited {
		return nil
	}
	if int64(len(payload)) > limit {
		return recoverablef(
			"%s: %s output is %d bytes (max %d); narrow the path or pattern, or raise %s",
			h.name, tool, len(payload), limit, h.policyKey("_max_output_bytes"),
		)
	}
	return nil
}

func (h *LocalFSBrowseTools) notFound(tool, userPath, absPath string) error {
	msg := fmt.Sprintf("%s: %s: %s does not exist", h.name, tool, userPath)
	if hint := didYouMean(filepath.Dir(absPath), filepath.Base(absPath)); hint != "" {
		msg += "." + hint
	}
	return recoverablef("%s", msg)
}

func (h *LocalFSBrowseTools) resolveTarget(ctx context.Context, tool, path string) (absPath, display string, info os.FileInfo, err error) {
	absPath, err = h.checkPath(ctx, path)
	if err != nil {
		return "", "", nil, err
	}
	if err = h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return "", "", nil, err
	}
	display = h.displayPath(ctx, absPath)
	info, statErr := os.Stat(absPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return absPath, display, nil, h.notFound(tool, path, absPath)
		}
		return absPath, display, nil, fmt.Errorf("%s: %s: %w", h.name, tool, statErr)
	}
	return absPath, display, info, nil
}

var _ taskengine.ToolsRepo = (*LocalFSBrowseTools)(nil)
