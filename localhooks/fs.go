package localhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

const localFSHookName = "local_fs"

// LocalFSHook provides direct filesystem access tools.
type LocalFSHook struct {
	allowedDir string
}

// NewLocalFSHook creates a new instance of LocalFSHook.
func NewLocalFSHook(allowedDir string) taskengine.HookRepo {
	return &LocalFSHook{
		allowedDir: filepath.Clean(allowedDir),
	}
}

// Exec handles filesystem tool execution.
func (h *LocalFSHook) Exec(ctx context.Context, startTime time.Time, input any, debug bool, hookCall *taskengine.HookCall) (any, taskengine.DataType, error) {
	if hookCall == nil {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: hook required")
	}

	args, ok := input.(map[string]any)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: input must be a map")
	}

	toolName := hookCall.ToolName
	if toolName == "" {
		toolName = hookCall.Name
	}

	switch toolName {
	case "read_file":
		return h.readFile(ctx, args)
	case "write_file":
		return h.writeFile(ctx, args)
	case "list_dir":
		return h.listDir(args)
	case "grep":
		return h.grep(ctx, args)
	case "sed":
		return h.sed(ctx, args)
	case "count_stats":
		return h.countStats(ctx, args)
	case "read_file_range":
		return h.readFileRange(ctx, args)
	case "stat_file":
		return h.statFile(args)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: unknown tool %s", toolName)
	}
}

// checkPath verifies if a path is within the allowed directory.
// It resolves symlinks so that a symlink inside the sandbox pointing outside it
// (e.g. ln -s /etc /allowed/link) is caught before any I/O is performed.
func (h *LocalFSHook) checkPath(path string) (string, error) {
	if h.allowedDir == "" {
		return "", errors.New("local_fs: no allowed directory configured")
	}

	absBase, err := filepath.Abs(h.allowedDir)
	if err != nil {
		return "", fmt.Errorf("local_fs: invalid allowed dir: %w", err)
	}

	absPath := path
	if !filepath.IsAbs(path) {
		absPath = filepath.Join(absBase, path)
	}
	absPath, err = filepath.Abs(absPath)
	if err != nil {
		return "", fmt.Errorf("local_fs: invalid path: %w", err)
	}

	// Resolve symlinks to find the true on-disk destination.
	// We only skip on NotExist so write_file to new files still works.
	realPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		absPath = realPath
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("local_fs: path resolution error: %w", err)
	}

	// Use the strict prefix check: ".." alone or "../" prefix.
	// strings.HasPrefix(rel, "..") would falsely trigger for "..hidden".
	sep := string(filepath.Separator)
	rel, err := filepath.Rel(absBase, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
		return "", fmt.Errorf("local_fs: path %s escapes allowed directory %s", path, h.allowedDir)
	}

	return absPath, nil
}

// maxReadBytesFromPolicy returns the max bytes for a full-file read. Non-positive means unlimited.
// Chain policy keys (hook_policies.local_fs): _max_read_bytes — default 1048576 (1 MiB) when unset.
func (h *LocalFSHook) maxReadBytesFromPolicy(ctx context.Context) (limit int64, unlimited bool) {
	args := taskengine.HookArgsFromContext(ctx, localFSHookName)
	if args == nil {
		return 1024 * 1024, false
	}
	s := strings.TrimSpace(args["_max_read_bytes"])
	if s == "" {
		return 1024 * 1024, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 1024 * 1024, false
	}
	if n <= 0 {
		return 0, true
	}
	return n, false
}

func (h *LocalFSHook) checkDeniedSubstrings(ctx context.Context, absPath string) error {
	base, err := filepath.Abs(h.allowedDir)
	if err != nil {
		return fmt.Errorf("local_fs: allowed dir: %w", err)
	}
	rel, err := filepath.Rel(base, absPath)
	if err != nil {
		return fmt.Errorf("local_fs: rel path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	args := taskengine.HookArgsFromContext(ctx, localFSHookName)
	if args == nil {
		return nil
	}
	raw := strings.TrimSpace(args["_denied_path_substrings"])
	if raw == "" {
		return nil
	}
	for _, pat := range strings.Split(raw, ",") {
		p := strings.TrimSpace(pat)
		if p == "" {
			continue
		}
		p = filepath.ToSlash(p)
		if strings.Contains(rel, p) {
			return fmt.Errorf("local_fs: path %q matches denied substring %q (hook_policies.local_fs._denied_path_substrings)", rel, p)
		}
	}
	return nil
}

func (h *LocalFSHook) checkFileSizeLimit(ctx context.Context, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("local_fs: stat: %w", err)
	}
	if info.IsDir() {
		return nil
	}
	limit, unlimited := h.maxReadBytesFromPolicy(ctx)
	if unlimited {
		return nil
	}
	if info.Size() > limit {
		return fmt.Errorf("local_fs: file is %d bytes (max %d); use read_file_range or set _max_read_bytes in hook_policies.local_fs", info.Size(), limit)
	}
	return nil
}

func (h *LocalFSHook) precheckFullRead(ctx context.Context, absPath string) error {
	if err := h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return err
	}
	return h.checkFileSizeLimit(ctx, absPath)
}

func (h *LocalFSHook) readFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for read_file")
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.precheckFullRead(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	return string(content), taskengine.DataTypeString, nil
}

func (h *LocalFSHook) writeFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for write_file")
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: content required for write_file")
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to create directories: %w", err)
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}

	return "ok", taskengine.DataTypeString, nil
}

func (h *LocalFSHook) listDir(args map[string]any) (any, taskengine.DataType, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read directory: %w", err)
	}

	var results []string
	for _, entry := range entries {
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		}
		results = append(results, entry.Name()+suffix)
	}

	return strings.Join(results, "\n"), taskengine.DataTypeString, nil
}

func (h *LocalFSHook) grep(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for grep")
	}
	pattern, ok := args["pattern"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: pattern required for grep")
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.precheckFullRead(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	var matches []string
	for i, line := range lines {
		if strings.Contains(line, pattern) {
			matches = append(matches, fmt.Sprintf("%d: %s", i+1, line))
		}
	}

	return strings.Join(matches, "\n"), taskengine.DataTypeString, nil
}

func (h *LocalFSHook) sed(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for sed")
	}
	pattern, ok := args["pattern"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: pattern required for sed")
	}
	replacement, ok := args["replacement"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: replacement required for sed")
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.precheckFullRead(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	newContent := strings.ReplaceAll(string(content), pattern, replacement)

	if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}

	return "ok", taskengine.DataTypeString, nil
}

func (h *LocalFSHook) countStats(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for count_stats")
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.precheckFullRead(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	lineCount := len(lines)
	if len(content) > 0 && content[len(content)-1] == '\n' {
		lineCount--
	}
	wordCount := len(strings.Fields(string(content)))
	byteCount := len(content)

	result := fmt.Sprintf("Lines: %d, Words: %d, Bytes: %d", lineCount, wordCount, byteCount)
	return result, taskengine.DataTypeString, nil
}

func (h *LocalFSHook) readFileRange(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for read_file_range")
	}
	startLine, ok := args["start_line"].(float64)
	if !ok {
		startLine = 1
	}
	endLine, ok := args["end_line"].(float64)

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	// Line-range reads still load the full file internally; enforce size to avoid multi-GB reads.
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	totalLines := len(lines)

	s := int(startLine)
	if s < 1 {
		s = 1
	}
	if s > totalLines {
		return "", taskengine.DataTypeString, nil
	}

	e := totalLines
	if ok {
		e = int(endLine)
	}
	if e < s {
		e = s
	}
	if e > totalLines {
		e = totalLines
	}

	resultLines := lines[s-1 : e]
	return strings.Join(resultLines, "\n"), taskengine.DataTypeString, nil
}

func (h *LocalFSHook) statFile(args map[string]any) (any, taskengine.DataType, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for stat_file")
	}

	absPath, err := h.checkPath(path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to stat file: %w", err)
	}

	result := map[string]any{
		"name":    info.Name(),
		"size":    info.Size(),
		"modTime": info.ModTime().Format(time.RFC3339),
		"isDir":   info.IsDir(),
	}

	b, _ := json.Marshal(result)
	return string(b), taskengine.DataTypeJSON, nil
}

func (h *LocalFSHook) Supports(ctx context.Context) ([]string, error) {
	return []string{localFSHookName, "read_file", "write_file", "list_dir", "grep", "sed", "count_stats", "read_file_range", "stat_file"}, nil
}

func (h *LocalFSHook) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

func (h *LocalFSHook) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	// If name is one of the sub-commands, return just that tool.
	// If name is "local_fs", return all of them.

	allTools := []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "read_file",
				Description: "Read the content of a file",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string", "description": "Path to the file relative to the project root"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "write_file",
				Description: "Write content to a file. Overwrites existing content. Creates directories if needed.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":    map[string]interface{}{"type": "string", "description": "Path to the file"},
						"content": map[string]interface{}{"type": "string", "description": "New content for the file"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "list_dir",
				Description: "List files in a directory",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string", "description": "Directory path (default: .)"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "grep",
				Description: "Find occurrences of a pattern in a file (line based)",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":    map[string]interface{}{"type": "string", "description": "Path to the file"},
						"pattern": map[string]interface{}{"type": "string", "description": "String to search for"},
					},
					"required": []string{"path", "pattern"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "sed",
				Description: "Replace occurrences of a pattern with a replacement in a file",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":        map[string]interface{}{"type": "string", "description": "Path to the file"},
						"pattern":     map[string]interface{}{"type": "string", "description": "String to replace"},
						"replacement": map[string]interface{}{"type": "string", "description": "Replacement string"},
					},
					"required": []string{"path", "pattern", "replacement"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "count_stats",
				Description: "Count lines, words, and bytes in a file (like wc)",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string", "description": "Path to the file"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "read_file_range",
				Description: "Read a specific range of lines from a file",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path":       map[string]interface{}{"type": "string", "description": "Path to the file"},
						"start_line": map[string]interface{}{"type": "integer", "description": "Starting line number (1-indexed, default 1)"},
						"end_line":   map[string]interface{}{"type": "integer", "description": "Ending line number (inclusive, optional)"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "stat_file",
				Description: "Get file metadata",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string", "description": "Path to the file/directory"},
					},
					"required": []string{"path"},
				},
			},
		},
	}

	if name == localFSHookName {
		return allTools, nil
	}

	for _, t := range allTools {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}

	return nil, fmt.Errorf("unknown hook tool: %s", name)
}

var _ taskengine.HookRepo = (*LocalFSHook)(nil)
