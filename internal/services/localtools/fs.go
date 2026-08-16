package localtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
	libdb "github.com/contenox/contenox/libdbexec"
)

const LocalFSToolsName = "local_fs"

const readBeforeWriteDenial = "local_fs: cannot modify existing file %s without reading it first. Call local_fs.read_file(%q) to confirm the current contents, then retry. " + severityRecoverable

const fileUnchangedStub = "File unchanged since last read — the content from your earlier read_file call in this conversation is still current. Pass force=true if you need the content re-sent."

const readBeforeWriteFullReadDenial = "local_fs: cannot overwrite existing file %s after only reading a line range. Call local_fs.read_file(%q) to read the full current contents, then retry. " + severityRecoverable

const readBeforeWriteStaleReadDenial = "local_fs: cannot modify existing file %s because it changed since you read it. Call local_fs.read_file(%q) to refresh the current contents, then retry. " + severityRecoverable

// FsUnchangedResult is read_file's session-dedup answer. See fileUnchangedStub.
type FsUnchangedResult struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Bytes     int    `json:"bytes"`
	Unchanged bool   `json:"unchanged"`

	content string
}

func (r FsUnchangedResult) String() string { return fileUnchangedStub }

// ProgramText returns the content the stub stood in for, so a non-model
// caller never mistakes the dedup notice for the file itself.
func (r FsUnchangedResult) ProgramText() (string, bool) { return r.content, true }

// FsRefusalResult is a soft refusal: a mutation that did not happen, reported
// to the model as an ordinary tool result so it can correct itself and retry.
type FsRefusalResult struct {
	Refused bool   `json:"refused"`
	Reason  string `json:"reason"`
}

func (r FsRefusalResult) String() string { return r.Reason }

// ProgramUnusable declares that there is no result here for a program to read —
// only an explanation of why nothing happened.
func (r FsRefusalResult) ProgramUnusable() string { return r.Reason }

func fsRefusal(reason string) FsRefusalResult {
	return FsRefusalResult{Refused: true, Reason: reason}
}

type readRequirement int

const (
	requireAnyFileRead readRequirement = iota
	requireFullFileRead
)

// StatFileIO is an optional interface a FileIO implementation may satisfy to
// serve metadata lookups; without it, this package falls back to os.Stat.
type StatFileIO interface {
	Stat(ctx context.Context, path string) (os.FileInfo, error)
}

// LocalFSTools provides direct filesystem access tools, gated by a
// per-session read-before-write history.
type LocalFSTools struct {
	allowedDir    string
	db            libdb.DBManager
	fileIO        FileIO
	name          string
	cwdResolver   func(context.Context) string
	dialect       SQLDialect
	onFileMutated func(absPath string)

}

// Option customises a LocalFSTools instance.
type FSOption func(*LocalFSTools)

// WithSQLDialect selects the placeholder style for the read-tracking table;
// defaults to DialectSQLite.
func WithSQLDialect(d SQLDialect) FSOption {
	return func(h *LocalFSTools) { h.dialect = d }
}

// WithOnFileMutated registers a callback fired with the absolute path after
// every successful write_file, sed, or edit_file; nil by default.
func WithOnFileMutated(fn func(absPath string)) FSOption {
	return func(h *LocalFSTools) { h.onFileMutated = fn }
}

func (h *LocalFSTools) notifyFileMutated(absPath string) {
	if h.onFileMutated != nil {
		h.onFileMutated(absPath)
	}
}

// NewLocalFSTools creates a LocalFSTools; db may be nil, which degrades the
// read-before-write guard to a no-op.
func NewLocalFSTools(allowedDir string, db libdb.DBManager) taskengine.ToolsRepo {
	return NewLocalFSToolsWith(allowedDir, db, nil, LocalFSToolsName, nil)
}

func NewLocalFSToolsWith(allowedDir string, db libdb.DBManager, io FileIO, name string, cwdResolver func(context.Context) string, opts ...FSOption) taskengine.ToolsRepo {
	if io == nil {
		io = noFilesystemIO{}
	}
	if name == "" {
		name = LocalFSToolsName
	}
	cleaned := allowedDir
	if cleaned != "" {
		cleaned = filepath.Clean(cleaned)
	}
	h := &LocalFSTools{
		allowedDir:  cleaned,
		db:          db,
		fileIO:      io,
		name:        name,
		cwdResolver: cwdResolver,
		dialect:     DialectSQLite,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Exec wraps execDispatch and stamps every returned error fatal-vs-recoverable.
func (h *LocalFSTools) Exec(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	res, dt, err := h.execDispatch(ctx, startTime, input, debug, toolsCall)
	return res, dt, markSeverity(err)
}

func (h *LocalFSTools) execDispatch(ctx context.Context, startTime time.Time, input any, debug bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if toolsCall == nil {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: tools required")
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
			return nil, taskengine.DataTypeAny, errors.New("local_fs: input must be a map (or provide tools.args)")
		}
	}

	toolName := toolsCall.ToolName
	if toolName == "" {
		toolName = toolsCall.Name
	}

	switch toolName {
	case "read_file":
		if err := rejectUnknownArgs("local_fs.read_file", args, "path", "start_line", "end_line", "force"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.readFile(ctx, args)
	case "write_file":
		if err := rejectUnknownArgs("local_fs.write_file", args, "path", "content"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.writeFile(ctx, args)
	case "edit_file":
		if err := rejectUnknownArgs("local_fs.edit_file", args, "path", "old_string", "new_string", "replace_all"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.editFile(ctx, args)
	case "sed":
		if err := rejectUnknownArgs("local_fs.sed", args, "path", "pattern", "replacement", "all", "expect_replacements", "start_line", "end_line"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.sed(ctx, args)
	case "read_file_range":
		if err := rejectUnknownArgs("local_fs.read_file_range", args, "path", "start_line", "end_line"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.readFileRange(ctx, args)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: unknown tool %s", toolName)
	}
}

func (h *LocalFSTools) baseDir(ctx context.Context) (string, error) {
	if args := h.policyArgs(ctx); len(args) > 0 {
		if policyDir := strings.TrimSpace(args["_allowed_dir"]); policyDir != "" {
			cleaned := filepath.Clean(policyDir)
			if filepath.IsAbs(cleaned) {
				return cleaned, nil
			}
			if cwd := h.resolveCwd(ctx); cwd != "" {
				return filepath.Clean(filepath.Join(cwd, cleaned)), nil
			}
			// Must not fall through to OS path resolution: that would silently
			// scope calls to this process's cwd rather than refusing outright.
			return "", fmt.Errorf(
				"local_fs: tools_policies.local_fs._allowed_dir %q is relative but no session workspace root could be resolved to anchor it "+
					"(this run has no live cwd resolver and its checkpoint carries no restored workspace root); "+
					"set _allowed_dir to an absolute path, or resume from a process that can restore the session's workspace root",
				policyDir)
		}
	}
	base := h.allowedDir
	if base == "" {
		base = h.resolveCwd(ctx)
	}
	if base == "" {
		return "", errors.New("local_fs: no allowed directory configured")
	}
	return base, nil
}

func (h *LocalFSTools) resolveCwd(ctx context.Context) string {
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

func (h *LocalFSTools) absAllowedDir(ctx context.Context) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.ResolveRoot(base)
	if err != nil {
		return "", fmt.Errorf("local_fs: invalid allowed dir: %w", err)
	}
	return resolved, nil
}

func (h *LocalFSTools) checkPath(ctx context.Context, path string) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.Contain(base, path)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			return "", fmt.Errorf("local_fs: path %s escapes allowed directory %s", path, base)
		}
		return "", fmt.Errorf("local_fs: %w", err)
	}
	return resolved, nil
}

func (h *LocalFSTools) displayPath(ctx context.Context, absPath string) string {
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

func (h *LocalFSTools) stat(ctx context.Context, absPath string) (os.FileInfo, error) {
	if s, ok := h.fileIO.(StatFileIO); ok {
		return s.Stat(ctx, absPath)
	}
	return os.Stat(absPath)
}

func (h *LocalFSTools) checkDeniedSubstrings(ctx context.Context, absPath string) error {
	subs := h.deniedSubstringsFromPolicy(ctx)
	if len(subs) == 0 {
		return nil
	}
	rel := h.displayPath(ctx, absPath)
	for _, p := range subs {
		if strings.Contains(rel, p) {
			return fmt.Errorf("local_fs: path %q matches denied substring %q (tools_policies.local_fs._denied_path_substrings)", rel, p)
		}
	}
	return nil
}

func (h *LocalFSTools) checkFileSizeLimit(ctx context.Context, absPath string) error {
	info, err := h.stat(ctx, absPath)
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
		return recoverablef("local_fs: file is %d bytes (max %d); use read_file_range or set _max_read_bytes in tools_policies.local_fs", info.Size(), limit)
	}
	return nil
}

func (h *LocalFSTools) precheckFullRead(ctx context.Context, absPath string) error {
	if err := h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return err
	}
	return h.checkFileSizeLimit(ctx, absPath)
}

func (h *LocalFSTools) refuseBinary(ctx context.Context, tool, displayPath, absPath string) error {
	content, err := h.fileIO.ReadFile(ctx, absPath)
	binary := false
	if err == nil {
		sample := content
		if len(sample) > sniffBinaryBytes {
			sample = sample[:sniffBinaryBytes]
		}
		binary = sniffBinarySample(sample)
	}
	if err != nil || !binary {
		return nil
	}
	return h.binaryRefusalError(ctx, tool, displayPath, absPath)
}

func (h *LocalFSTools) binaryRefusalError(ctx context.Context, tool, displayPath, absPath string) error {
	detail := "binary file"
	if info, statErr := h.stat(ctx, absPath); statErr == nil {
		detail = fmt.Sprintf("binary file (%s)", fileSizeAndExecFlag(info))
	}
	return recoverablef(
		"local_fs: %s: refusing to read %s: %s. Use shell tools for binaries.",
		tool, displayPath, detail)
}

func (h *LocalFSTools) checkToolOutputLimit(ctx context.Context, tool string, payload string) error {
	limit, unlimited := h.maxOutputBytesFromPolicy(ctx)
	if unlimited {
		return nil
	}
	if int64(len(payload)) > limit {
		return recoverablef(
			"local_fs: %s output is %d bytes (max %d); narrow the path or pattern, use read_file_range, or set _max_output_bytes in tools_policies.local_fs",
			tool, len(payload), limit,
		)
	}
	return nil
}

func (h *LocalFSTools) notFound(tool, userPath, absPath string) error {
	msg := fmt.Sprintf("local_fs: %s: %s does not exist", tool, userPath)
	return recoverablef("%s", msg)
}

func (h *LocalFSTools) resolveTarget(ctx context.Context, tool, path string) (absPath, display string, info os.FileInfo, err error) {
	absPath, err = h.checkPath(ctx, path)
	if err != nil {
		return "", "", nil, err
	}
	if err = h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return "", "", nil, err
	}
	display = h.displayPath(ctx, absPath)
	info, statErr := h.stat(ctx, absPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return absPath, display, nil, h.notFound(tool, path, absPath)
		}
		return absPath, display, nil, fmt.Errorf("local_fs: %s: %w", tool, statErr)
	}
	return absPath, display, info, nil
}

func (h *LocalFSTools) readFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for read_file")
	}

	absPath, display, info, err := h.resolveTarget(ctx, "read_file", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: read_file: %s is a directory (%s)", display, describePathForError(absPath, info))
	}

	if _, hasStart := argFloat(args, "start_line"); hasStart {
		return h.readFileRange(ctx, args)
	}
	if _, hasEnd := argFloat(args, "end_line"); hasEnd {
		return h.readFileRange(ctx, args)
	}

	outLimit, unlimitedOut := h.maxOutputBytesFromPolicy(ctx)
	readLimit, unlimitedRead := h.maxReadBytesFromPolicy(ctx)

	// A file over the read cap cannot be loaded whole; name the exact next step.
	if !unlimitedRead && info.Size() > readLimit {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: read_file: %s is %s (%d bytes), over the %d-byte read cap; read a portion with read_file start_line/end_line (e.g. start_line: 1), or raise _max_read_bytes in tools_policies.local_fs to load the whole file",
			display, humanSize(info.Size()), info.Size(), readLimit)
	}

	content, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}
	// Belt and braces: a file whose first 512 bytes are ASCII but whose body is
	// not still should not reach the transcript.
	if isBinarySample(sniffPrefix(content)) {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: read_file: refusing to read %s: binary file (%s). Use shell tools for binaries.",
			display, fileSizeAndExecFlag(info))
	}

	force, _ := argBool(args, "force")
	hash := contentHash(content)
	if !force && !h.readTrackingDisabled(ctx) && h.hasCurrentFullRead(ctx, absPath, hash) {
		return FsUnchangedResult{
			Path:      display,
			SHA256:    hash,
			Bytes:     len(content),
			Unchanged: true,
			content:   string(content),
		}, taskengine.DataTypeString, nil
	}

	out := string(content)
	if !unlimitedOut && int64(len(out)) > outLimit {
		// A truncated read has not seen the whole file and must not authorize
		// a blind overwrite, so only a range read is recorded here.
		head, lastLine, nextLine, _ := streamRange(bytes.NewReader(content), 1, math.MaxInt, outLimit)
		total := countTextLines(out)
		h.recordRangeRead(ctx, absPath, content)
		notice := fmt.Sprintf(
			"local_fs: read_file truncated — showed lines 1-%d of %d; output capped at %d bytes. To read the next page call read_file with start_line: %d. %s",
			lastLine, total, outLimit, nextLine, severityRecoverable,
		)
		return head + "\n\n" + notice, taskengine.DataTypeString, nil
	}
	h.recordFullRead(ctx, absPath, content)
	return out, taskengine.DataTypeString, nil
}

func (h *LocalFSTools) readFileRange(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for read_file_range")
	}
	start := 1
	if v, ok := argInt(args, "start_line"); ok {
		if start = v; start < 1 {
			start = 1
		}
	}
	end := math.MaxInt
	if v, ok := argInt(args, "end_line"); ok {
		end = v
	}
	if end < start {
		end = start
	}

	absPath, display, info, err := h.resolveTarget(ctx, "read_file_range", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: read_file_range: %s is a directory", display)
	}
	if err := h.refuseBinary(ctx, "read_file_range", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	outLimit, unlimitedOut := h.maxOutputBytesFromPolicy(ctx)
	budget := int64(0)
	if !unlimitedOut && outLimit > 0 {
		budget = outLimit
	}

	content, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	totalLines := len(lines)
	if start > totalLines {
		h.recordRangeRead(ctx, absPath, content)
		return "", taskengine.DataTypeString, nil
	}
	e := end
	if e > totalLines {
		e = totalLines
	}
	out := strings.Join(lines[start-1:e], "\n")
	h.recordRangeRead(ctx, absPath, content)

	if budget > 0 && int64(len(out)) > budget {
		// streamRange here is fed the extracted range from line 1, so its
		// numbers must be rebased onto `start`.
		head, lastLine, nextLine, _ := streamRange(bytes.NewReader([]byte(out)), 1, math.MaxInt, budget)
		absNext := start + nextLine - 1
		notice := fmt.Sprintf(
			"local_fs: read_file_range truncated — showed lines %d-%d; output capped at %d bytes. To read the next page call read_file with start_line: %d. %s",
			start, start+lastLine-1, budget, absNext, severityRecoverable)
		return head + "\n\n" + notice, taskengine.DataTypeString, nil
	}
	return out, taskengine.DataTypeString, nil
}

type FsWriteResult struct {
	Path      string `json:"path"`
	Written   bool   `json:"written"`
	OldBytes  int    `json:"old_bytes"`
	NewBytes  int    `json:"new_bytes"`
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
	AbsPath   string `json:"-"`
	OldText   string `json:"-"`
	NewText   string `json:"-"`
}

func (r FsWriteResult) ToolDiff() (string, string, string, bool) {
	return toolDiff(r.AbsPath, r.Path, r.Written, r.OldText, r.NewText)
}

type FsSedResult struct {
	Path         string `json:"path"`
	Written      bool   `json:"written"`
	Changed      bool   `json:"changed"`
	Replacements int    `json:"replacements"`
	OldBytes     int    `json:"old_bytes"`
	NewBytes     int    `json:"new_bytes"`
	OldSHA256    string `json:"old_sha256"`
	NewSHA256    string `json:"new_sha256"`
	AbsPath      string `json:"-"`
	OldText      string `json:"-"`
	NewText      string `json:"-"`
}

func (r FsSedResult) ToolDiff() (string, string, string, bool) {
	return toolDiff(r.AbsPath, r.Path, r.Written, r.OldText, r.NewText)
}

func toolDiff(absPath, path string, written bool, oldText, newText string) (string, string, string, bool) {
	if absPath == "" {
		absPath = path
	}
	if absPath == "" || !written || oldText == newText {
		return "", "", "", false
	}
	return absPath, oldText, newText, true
}

func (h *LocalFSTools) writeFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for write_file")
	}
	content, ok := argString(args, "content")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: content required for write_file")
	}

	absPath, err := h.checkPath(ctx, path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.checkDeniedSubstrings(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	display := h.displayPath(ctx, absPath)

	gate := h.requireReadBeforeMutation(ctx, absPath, display, requireFullFileRead)
	if gate.denied {
		return fsRefusal(gate.denial), taskengine.DataTypeString, nil
	}
	// gate already read the file to validate its hash; reuse those bytes.
	oldBytes := gate.content

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to create directories for %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to create directories: %w", err)
	}

	// Re-hash immediately before overwriting to close the validate-then-write
	// race as far as a single process can.
	if gate.exists && gate.verified && !h.readTrackingDisabled(ctx) {
		if unchanged, _ := h.confirmUnchanged(ctx, absPath, gate.hash); !unchanged {
			h.invalidateReads(ctx, absPath)
			return fsRefusal(fmt.Sprintf(readBeforeWriteStaleReadDenial, display, display)), taskengine.DataTypeString, nil
		}
	}

	if err := h.writeFileDurable(ctx, absPath, []byte(content)); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to write file %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}
	h.invalidateReads(ctx, absPath)
	h.notifyFileMutated(absPath)

	return FsWriteResult{
		Path:      display,
		AbsPath:   absPath,
		Written:   true,
		OldBytes:  len(oldBytes),
		NewBytes:  len(content),
		OldSHA256: contentHash(oldBytes),
		NewSHA256: contentHash([]byte(content)),
		OldText:   string(oldBytes),
		NewText:   content,
	}, taskengine.DataTypeJSON, nil
}

// FsEditResult is edit_file's success result: exact-string replacement, unlike
// write_file's full overwrite or sed's regexless-but-scoped substitution.
type FsEditResult struct {
	Path         string `json:"path"`
	Written      bool   `json:"written"`
	Replacements int    `json:"replacements"`
	OldBytes     int    `json:"old_bytes"`
	NewBytes     int    `json:"new_bytes"`
	OldSHA256    string `json:"old_sha256"`
	NewSHA256    string `json:"new_sha256"`
	AbsPath      string `json:"-"`
	OldText      string `json:"-"`
	NewText      string `json:"-"`
}

func (r FsEditResult) ToolDiff() (string, string, string, bool) {
	return toolDiff(r.AbsPath, r.Path, r.Written, r.OldText, r.NewText)
}

func (h *LocalFSTools) editFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for edit_file")
	}
	oldString, ok := argString(args, "old_string")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: old_string required for edit_file")
	}
	if oldString == "" {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: edit_file: old_string must not be empty")
	}
	newString, ok := argString(args, "new_string")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: new_string required for edit_file")
	}
	if oldString == newString {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: edit_file: old_string and new_string are identical; nothing to change")
	}
	replaceAll, _ := argBool(args, "replace_all")

	absPath, display, info, err := h.resolveTarget(ctx, "edit_file", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: edit_file: %s is a directory", display)
	}
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.refuseBinary(ctx, "edit_file", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	gate := h.requireReadBeforeMutation(ctx, absPath, display, requireAnyFileRead)
	if gate.denied {
		return fsRefusal(gate.denial), taskengine.DataTypeString, nil
	}
	if !gate.verified {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: edit_file: could not read %s", display)
	}
	oldText := string(gate.content)

	count := strings.Count(oldText, oldString)
	if count == 0 {
		return recoverablef(
			"local_fs: edit_file: old_string not found in %s — the file may differ from what you expect; call read_file and retry with the exact current text",
			display).Error(), taskengine.DataTypeString, nil
	}
	if !replaceAll && count > 1 {
		return recoverablef(
			"local_fs: edit_file: old_string occurs %d times in %s; add more surrounding context to make it unique, or set replace_all=true to replace every occurrence",
			count, display).Error(), taskengine.DataTypeString, nil
	}

	newText := strings.ReplaceAll(oldText, oldString, newString)

	if !h.readTrackingDisabled(ctx) {
		if unchanged, _ := h.confirmUnchanged(ctx, absPath, gate.hash); !unchanged {
			h.invalidateReads(ctx, absPath)
			return fsRefusal(fmt.Sprintf(readBeforeWriteStaleReadDenial, display, display)), taskengine.DataTypeString, nil
		}
	}

	if err := h.writeFileDurable(ctx, absPath, []byte(newText)); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to write file %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}
	h.invalidateReads(ctx, absPath)
	h.notifyFileMutated(absPath)

	newBytes := []byte(newText)
	return FsEditResult{
		Path:         display,
		AbsPath:      absPath,
		Written:      true,
		Replacements: count,
		OldBytes:     len(gate.content),
		NewBytes:     len(newBytes),
		OldSHA256:    gate.hash,
		NewSHA256:    contentHash(newBytes),
		OldText:      oldText,
		NewText:      newText,
	}, taskengine.DataTypeJSON, nil
}

func occurrenceLines(text, pattern string, lineOffset, max int) []int {
	var out []int
	for i, line := range strings.Split(text, "\n") {
		if strings.Contains(line, pattern) {
			out = append(out, i+1+lineOffset)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

func (h *LocalFSTools) sed(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for sed")
	}
	pattern, ok := argString(args, "pattern")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: pattern required for sed")
	}
	if pattern == "" {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: sed: pattern must not be empty")
	}
	replacement, ok := argString(args, "replacement")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: replacement required for sed")
	}
	replaceAll, _ := argBool(args, "all")
	expected, hasExpected := argInt(args, "expect_replacements")

	absPath, display, info, err := h.resolveTarget(ctx, "sed", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: sed: %s is a directory", display)
	}
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.refuseBinary(ctx, "sed", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	gate := h.requireReadBeforeMutation(ctx, absPath, display, requireAnyFileRead)
	if gate.denied {
		return fsRefusal(gate.denial), taskengine.DataTypeString, nil
	}
	if !gate.verified {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: sed: could not read %s", display)
	}
	content := gate.content
	oldText := string(content)

	lines := strings.Split(oldText, "\n")
	scopeStart, scopeEnd := 1, len(lines)
	if v, ok := argInt(args, "start_line"); ok && v > 1 {
		scopeStart = v
	}
	if v, ok := argInt(args, "end_line"); ok && v >= scopeStart {
		scopeEnd = v
	}
	if scopeEnd > len(lines) {
		scopeEnd = len(lines)
	}
	scoped := scopeStart > 1 || scopeEnd < len(lines)
	if scopeStart > len(lines) {
		return recoverablef("local_fs: sed: start_line %d is past the end of %s (%d lines); file left unchanged",
			scopeStart, display, len(lines)).Error(), taskengine.DataTypeString, nil
	}

	window := strings.Join(lines[scopeStart-1:scopeEnd], "\n")
	count := strings.Count(window, pattern)

	if count == 0 {
		// A fuzzy match is never applied: a misplaced edit is corruption.
		msg := fmt.Sprintf(
			"local_fs: sed: pattern %q not found in %s — file left unchanged; correct the pattern and retry. %s",
			pattern, display, severityRecoverable)
		if near := suggestNearestLines(window, pattern, 2); near != "" {
			msg += "\nClosest lines:\n" + near
		}
		return msg, taskengine.DataTypeString, nil
	}

	// Ambiguous matches are refused rather than guessed at: an explicit
	// all=true or expect_replacements=N is required for a wide edit.
	switch {
	case hasExpected && count != expected:
		at := occurrenceLines(window, pattern, scopeStart-1, 5)
		return recoverablef(
			"local_fs: sed: expected %d occurrences of %q in %s but found %d (near lines %v); file left unchanged",
			expected, pattern, display, count, at).Error(), taskengine.DataTypeString, nil
	case !hasExpected && !replaceAll && count > 1:
		at := occurrenceLines(window, pattern, scopeStart-1, 5)
		return recoverablef(
			"local_fs: sed: pattern %q occurs %d times in %s (near lines %v); file left unchanged. Extend the pattern to something unique, narrow it with start_line/end_line, or pass all=true to replace every occurrence",
			pattern, count, display, at).Error(), taskengine.DataTypeString, nil
	}

	newWindow := strings.ReplaceAll(window, pattern, replacement)
	var newContent string
	if scoped {
		out := make([]string, 0, len(lines))
		out = append(out, lines[:scopeStart-1]...)
		out = append(out, strings.Split(newWindow, "\n")...)
		out = append(out, lines[scopeEnd:]...)
		newContent = strings.Join(out, "\n")
	} else {
		newContent = newWindow
	}

	if !h.readTrackingDisabled(ctx) {
		if unchanged, _ := h.confirmUnchanged(ctx, absPath, gate.hash); !unchanged {
			h.invalidateReads(ctx, absPath)
			return fsRefusal(fmt.Sprintf(readBeforeWriteStaleReadDenial, display, display)), taskengine.DataTypeString, nil
		}
	}

	if err := h.writeFileDurable(ctx, absPath, []byte(newContent)); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to write file %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}
	h.invalidateReads(ctx, absPath)
	h.notifyFileMutated(absPath)

	newBytes := []byte(newContent)
	return FsSedResult{
		Path:         display,
		AbsPath:      absPath,
		Written:      true,
		Changed:      newContent != oldText,
		Replacements: count,
		OldBytes:     len(content),
		NewBytes:     len(newBytes),
		OldSHA256:    gate.hash,
		NewSHA256:    contentHash(newBytes),
		OldText:      oldText,
		NewText:      newContent,
	}, taskengine.DataTypeJSON, nil
}

func grepLineRange(args map[string]any, numLines int) (start, end int) {
	start = 1
	end = numLines
	if v, ok := argInt(args, "start_line"); ok {
		if v < 1 {
			v = 1
		}
		start = v
	}
	if v, ok := argInt(args, "end_line"); ok {
		if v < start {
			v = start
		}
		end = v
	}
	if end > numLines {
		end = numLines
	}
	if start > numLines {
		start = numLines + 1
	}
	if end < start {
		end = start - 1
	}
	return start, end
}

const dirGrepMaxMatches = 100

func doubleStarGlobMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "**") {
		matched, _ := filepath.Match(pattern, s)
		return matched
	}
	idx := strings.Index(pattern, "**")
	prefix := strings.TrimSuffix(pattern[:idx], "/")
	after := strings.TrimPrefix(pattern[idx+2:], "/")

	if prefix != "" {
		if s == prefix {
			return after == ""
		}
		if !strings.HasPrefix(s, prefix+"/") {
			return false
		}
		s = s[len(prefix)+1:]
	}
	if after == "" {
		return true
	}
	for {
		if doubleStarGlobMatch(after, s) {
			return true
		}
		slash := strings.Index(s, "/")
		if slash < 0 {
			break
		}
		s = s[slash+1:]
	}
	return false
}

var _ taskengine.ToolsRepo = (*LocalFSTools)(nil)
