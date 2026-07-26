package localtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/vfs"
)

const LocalFSToolsName = "local_fs"

// readBeforeWriteDenial is the LLM-facing message returned when the model tries
// to mutate an existing file it has not read in this session. The model treats
// it as a normal tool result and is expected to call read_file then retry. It
// carries the Rec 5 recoverable-by-correction severity marker like every other
// correctable local-tools condition.
//
// The path substituted here is the project-relative path, not the host
// absolute path: the schema tells the model paths are relative to the project
// root, so instructing it to call read_file("/abs/host/path") is an
// instruction it may not be able to follow.
const readBeforeWriteDenial = "local_fs: cannot modify existing file %s without reading it first. Call local_fs.read_file(%q) to confirm the current contents, then retry. " + severityRecoverable

// fileUnchangedStub is the tool-result text returned when the model re-reads a
// file whose content hash is already recorded in this session. The earlier
// read_file result is still in the conversation context, so re-sending the full
// content wastes tokens without providing new information.
//
// This holds only while that earlier result is still in context. Call
// InvalidateSessionReads after compaction, or pass force=true.
const fileUnchangedStub = "File unchanged since last read — the content from your earlier read_file call in this conversation is still current. Pass force=true if you need the content re-sent."

const readBeforeWriteFullReadDenial = "local_fs: cannot overwrite existing file %s after only reading a line range. Call local_fs.read_file(%q) to read the full current contents, then retry. " + severityRecoverable

const readBeforeWriteStaleReadDenial = "local_fs: cannot modify existing file %s because it changed since you read it. Call local_fs.read_file(%q) to refresh the current contents, then retry. " + severityRecoverable

type readRequirement int

const (
	// requireAnyFileRead is enough for targeted mutators such as sed.
	requireAnyFileRead readRequirement = iota
	// requireFullFileRead is required for full-file overwrite via write_file.
	requireFullFileRead
)

// StatFileIO is an optional interface a FileIO implementation may satisfy to
// serve metadata lookups. Without it, this package falls back to os.Stat.
//
// Previously every stat went straight to os.Stat while reads went through
// FileIO, so a test injecting a mock FileIO still hit the real filesystem for
// metadata — the abstraction was only half applied.
type StatFileIO interface {
	Stat(ctx context.Context, path string) (os.FileInfo, error)
}

// LocalFSTools provides direct filesystem access tools.
//
// The tool tracks its own per-session read history in the local_fs_reads table
// so that write_file / sed against an existing file can be blocked unless that
// file has been read first this session. State ownership lives entirely with
// this tool — the engine never sees the rule.
type LocalFSTools struct {
	allowedDir  string
	db          libdb.DBManager
	fileIO      FileIO
	name        string
	cwdResolver func(context.Context) string
	dialect     SQLDialect
	fileIOIsOS  bool
}

// Option customises a LocalFSTools instance.
type FSOption func(*LocalFSTools)

// WithSQLDialect selects the placeholder style for the read-tracking table.
// Defaults to DialectSQLite.
func WithSQLDialect(d SQLDialect) FSOption {
	return func(h *LocalFSTools) { h.dialect = d }
}

// NewLocalFSTools creates a new instance of LocalFSTools. db may be nil; when
// nil, the read-before-write guard degrades to a no-op (used by tests and
// callers without a DB).
func NewLocalFSTools(allowedDir string, db libdb.DBManager) taskengine.ToolsRepo {
	return NewLocalFSToolsWith(allowedDir, db, nil, LocalFSToolsName, nil)
}

func NewLocalFSToolsWith(allowedDir string, db libdb.DBManager, io FileIO, name string, cwdResolver func(context.Context) string, opts ...FSOption) taskengine.ToolsRepo {
	if io == nil {
		io = osFileIO{}
	}
	_, isOS := io.(osFileIO)
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
		fileIOIsOS:  isOS,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Exec handles filesystem tool execution. It is a thin wrapper over
// execDispatch that stamps every returned error with a fatal-vs-recoverable
// severity marker (Rec 5, tool-hardening.md): callers can key on
// "(recoverable: ...)" vs "(fatal: ...)" to decide whether a corrected retry is
// worthwhile. Soft tool-result strings (read-before-write denials, sed no-match
// suggestions) carry their own recoverable marker inline and flow through the
// nil-error path untouched here.
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
		// Declarative `tools` tasks carry their arguments on the ToolsCall
		// (like local_shell); fall back to them when the chain input isn't an
		// args map (e.g. chat history flowing through a gated tool task).
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
	case "list_dir":
		if err := rejectUnknownArgs("local_fs.list_dir", args, "path", "recursive", "max_depth", "offset"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.listDir(ctx, args)
	case "grep":
		if err := rejectUnknownArgs("local_fs.grep", args, "path", "pattern", "regex", "start_line", "end_line"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.grep(ctx, args)
	case "find_files":
		if err := rejectUnknownArgs("local_fs.find_files", args, "pattern", "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.findFiles(ctx, args)
	case "sed":
		if err := rejectUnknownArgs("local_fs.sed", args, "path", "pattern", "replacement", "all", "expect_replacements", "start_line", "end_line"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.sed(ctx, args)
	case "count_stats":
		if err := rejectUnknownArgs("local_fs.count_stats", args, "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.countStats(ctx, args)
	case "read_file_range":
		if err := rejectUnknownArgs("local_fs.read_file_range", args, "path", "start_line", "end_line"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.readFileRange(ctx, args)
	case "stat_file":
		if err := rejectUnknownArgs("local_fs.stat_file", args, "path"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return h.statFile(ctx, args)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: unknown tool %s", toolName)
	}
}

// ---------------------------------------------------------------------------
// Path handling
// ---------------------------------------------------------------------------

func (h *LocalFSTools) baseDir(ctx context.Context) (string, error) {
	if args := h.policyArgs(ctx); len(args) > 0 {
		if policyDir := strings.TrimSpace(args["_allowed_dir"]); policyDir != "" {
			cleaned := filepath.Clean(policyDir)
			if filepath.IsAbs(cleaned) {
				return cleaned, nil
			}
			if h.cwdResolver != nil {
				if cwd := h.cwdResolver(ctx); cwd != "" {
					return filepath.Clean(filepath.Join(cwd, cleaned)), nil
				}
			}
			return cleaned, nil
		}
	}
	base := h.allowedDir
	if base == "" && h.cwdResolver != nil {
		if r := h.cwdResolver(ctx); r != "" {
			base = filepath.Clean(r)
		}
	}
	if base == "" {
		return "", errors.New("local_fs: no allowed directory configured")
	}
	return base, nil
}

// absAllowedDir returns the symlink-resolved base directory for the current
// call context. Base resolution lives in the vfs package (the single home for
// workspace-root handling).
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

// checkPath verifies that a path is within the allowed directory. Containment —
// path normalization plus symlink-escape guarding — is delegated to the vfs
// package so there is a single implementation shared with the /files browse
// API. A symlink inside the sandbox pointing outside it (e.g. ln -s /etc
// /allowed/link) is caught before any I/O is performed.
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

// displayPath renders an absolute path the way the model is expected to
// address it: relative to the workspace root, forward-slashed. Every
// model-facing message uses this so the paths in errors are paths the model can
// paste straight back into the next call.
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

// stat routes metadata lookups through the FileIO abstraction when it supports
// them, and to the OS otherwise.
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

// refuseBinary rejects a content-consuming call against a file that sniffs as
// binary. Called before the file is loaded, so a 50 MB executable costs 512
// bytes of I/O rather than a full read that is then discarded.
func (h *LocalFSTools) refuseBinary(ctx context.Context, tool, displayPath, absPath string) error {
	binary, err := sniffBinaryFile(absPath)
	if err != nil || !binary {
		return nil
	}
	detail := "binary file"
	if info, statErr := h.stat(ctx, absPath); statErr == nil {
		detail = fmt.Sprintf("binary file (%s)", fileSizeAndExecFlag(info))
	}
	return recoverablef(
		"local_fs: %s: refusing to read %s: %s. Use stat_file or shell tools for binaries.",
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

// notFound builds a recoverable not-found error with a fuzzy "Did you mean:"
// clause over the parent directory's entries (Rec 7). Suggestion only — the tool
// never acts on a fuzzy match (the fuzzy law).
func (h *LocalFSTools) notFound(tool, userPath, absPath string) error {
	msg := fmt.Sprintf("local_fs: %s: %s does not exist", tool, userPath)
	if hint := didYouMean(filepath.Dir(absPath), filepath.Base(absPath)); hint != "" {
		msg += "." + hint
	}
	return recoverablef("%s", msg)
}

// resolveTarget performs the containment check, the denied-substring check, and
// the stat that nearly every tool needs, in one place.
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

// ---------------------------------------------------------------------------
// read_file / read_file_range
// ---------------------------------------------------------------------------

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
			"local_fs: read_file: %s is a directory (%s); use list_dir", display, describePathForError(absPath, info))
	}

	// Rec 4: read_file gained optional start_line/end_line, so a truncated read
	// can name "use start_line: N" actionable on the SAME tool. When either is
	// present this is a ranged (paging) read, delegated to the shared reader.
	if _, hasStart := argFloat(args, "start_line"); hasStart {
		return h.readFileRange(ctx, args)
	}
	if _, hasEnd := argFloat(args, "end_line"); hasEnd {
		return h.readFileRange(ctx, args)
	}

	// Sniff before loading: a 512-byte read settles this, whereas sniffing the
	// already-loaded buffer saves the context but not the I/O.
	if err := h.refuseBinary(ctx, "read_file", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	outLimit, unlimitedOut := h.maxOutputBytesFromPolicy(ctx)
	readLimit, unlimitedRead := h.maxReadBytesFromPolicy(ctx)

	// A file larger than the read cap cannot be loaded whole. Rec 4: don't dump
	// a partial; name the exact next step.
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
			"local_fs: read_file: refusing to read %s: binary file (%s). Use stat_file or shell tools for binaries.",
			display, fileSizeAndExecFlag(info))
	}

	// Dedup: if this session has already read this exact file version, return a
	// stub instead of re-sending the full content.
	force, _ := argBool(args, "force")
	hash := contentHash(content)
	if !force && !h.readTrackingDisabled(ctx) && h.hasCurrentFullRead(ctx, absPath, hash) {
		return fileUnchangedStub, taskengine.DataTypeString, nil
	}

	out := string(content)
	if !unlimitedOut && int64(len(out)) > outLimit {
		// Rec 4: never truncate silently. Return a line-based head that fits the
		// output cap and name the exact next step. The file on disk is its own
		// durable copy, so read_file does not spool — the model pages forward
		// with start_line. Records a RANGE read only: a truncated read has not
		// seen the whole file, so it must not authorize a blind overwrite.
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
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: read_file_range: %s is a directory; use list_dir", display)
	}
	if err := h.refuseBinary(ctx, "read_file_range", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	outLimit, unlimitedOut := h.maxOutputBytesFromPolicy(ctx)
	readLimit, unlimitedRead := h.maxReadBytesFromPolicy(ctx)
	budget := int64(0)
	if !unlimitedOut && outLimit > 0 {
		budget = outLimit
	}

	// Over the read cap: stream the requested range without loading the whole
	// file (Rec 4). No read marker recorded — an over-cap file cannot be loaded
	// for mutation, so the read-before-write gate is moot for it.
	if !unlimitedRead && info.Size() > readLimit {
		f, err := os.Open(absPath)
		if err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: read_file_range open: %w", err)
		}
		defer f.Close()
		// streamRange reports lastLine/nextLine as ABSOLUTE file line numbers
		// when fed the file from its start with a start offset.
		out, lastLine, nextLine, sErr := streamRange(f, start, end, budget)
		if sErr != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: read_file_range stream: %w", sErr)
		}
		// Only a BUDGET-driven early stop is a truncation: reaching the
		// requested end_line returned exactly what was asked for, even though
		// the file continues past it.
		if nextLine != 0 && lastLine < end {
			out += fmt.Sprintf(
				"\n\nlocal_fs: read_file_range truncated — showed lines %d-%d; output capped at %d bytes. To read the next page call read_file with start_line: %d. %s",
				start, lastLine, budget, nextLine, severityRecoverable)
		}
		return out, taskengine.DataTypeString, nil
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

	// Rec 4: truncate-and-continue on the output cap rather than erroring.
	if budget > 0 && int64(len(out)) > budget {
		// Here streamRange is fed the extracted range starting at line 1, so
		// its line numbers are RELATIVE to `start` and must be rebased.
		head, lastLine, nextLine, _ := streamRange(bytes.NewReader([]byte(out)), 1, math.MaxInt, budget)
		absNext := start + nextLine - 1
		notice := fmt.Sprintf(
			"local_fs: read_file_range truncated — showed lines %d-%d; output capped at %d bytes. To read the next page call read_file with start_line: %d. %s",
			start, start+lastLine-1, budget, absNext, severityRecoverable)
		return head + "\n\n" + notice, taskengine.DataTypeString, nil
	}
	return out, taskengine.DataTypeString, nil
}

// ---------------------------------------------------------------------------
// write_file / sed
// ---------------------------------------------------------------------------

type FsWriteResult struct {
	Path      string `json:"path"`
	Written   bool   `json:"written"`
	OldBytes  int    `json:"old_bytes"`
	NewBytes  int    `json:"new_bytes"`
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
	OldText   string `json:"-"`
	NewText   string `json:"-"`
}

func (r FsWriteResult) ToolDiff() (string, string, string, bool) {
	if r.Path == "" || !r.Written || r.OldText == r.NewText {
		return "", "", "", false
	}
	return r.Path, r.OldText, r.NewText, true
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
	OldText      string `json:"-"`
	NewText      string `json:"-"`
}

func (r FsSedResult) ToolDiff() (string, string, string, bool) {
	if r.Path == "" || !r.Written || r.OldText == r.NewText {
		return "", "", "", false
	}
	return r.Path, r.OldText, r.NewText, true
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
		return gate.denial, taskengine.DataTypeString, nil
	}
	// gate already read the file to validate its hash; reuse those bytes
	// instead of reading a second time.
	oldBytes := gate.content

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to create directories for %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to create directories: %w", err)
	}

	// Close the validate-then-overwrite window as far as a single process can:
	// re-hash immediately before replacing the bytes. Skipped when read
	// tracking is off (no DB or no session, e.g. one-shot `contenox run`),
	// where there is no read-before-write contract to enforce and a blind
	// write is the expected behaviour.
	if gate.exists && gate.verified && !h.readTrackingDisabled(ctx) {
		if unchanged, _ := h.confirmUnchanged(ctx, absPath, gate.hash); !unchanged {
			h.invalidateReads(ctx, absPath)
			return fmt.Sprintf(readBeforeWriteStaleReadDenial, display, display), taskengine.DataTypeString, nil
		}
	}

	if err := h.writeFileDurable(ctx, absPath, []byte(content)); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to write file %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}
	h.invalidateReads(ctx, absPath)

	return FsWriteResult{
		Path:      absPath,
		Written:   true,
		OldBytes:  len(oldBytes),
		NewBytes:  len(content),
		OldSHA256: contentHash(oldBytes),
		NewSHA256: contentHash([]byte(content)),
		OldText:   string(oldBytes),
		NewText:   content,
	}, taskengine.DataTypeJSON, nil
}

// occurrenceLines reports the 1-based line numbers on which pattern occurs,
// capped at max entries. lineOffset rebases the numbers onto the whole file
// when text is a scoped window rather than the full contents — reporting
// window-relative line numbers would send the model looking in the wrong place.
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
		return gate.denial, taskengine.DataTypeString, nil
	}
	if !gate.verified {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: sed: could not read %s", display)
	}
	content := gate.content
	oldText := string(content)

	// Optional line-range scoping, so an edit located with grep can be applied
	// exactly where it was found.
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
		// Rec 7 + the fuzzy law: the pattern was not found. SUGGEST the nearest
		// actual lines and DO NOT mutate — a fuzzy match must never be applied
		// silently, because a misplaced edit is corruption.
		msg := fmt.Sprintf(
			"local_fs: sed: pattern %q not found in %s — file left unchanged; correct the pattern and retry. %s",
			pattern, display, severityRecoverable)
		if near := suggestNearestLines(window, pattern, 2); near != "" {
			msg += "\nClosest lines:\n" + near
		}
		return msg, taskengine.DataTypeString, nil
	}

	// Ambiguity is refused rather than resolved.
	//
	// Unconditional ReplaceAll is the single most dangerous behaviour a
	// string-replace edit tool can have: replacing a common identifier silently
	// rewrites every call site and reports the damage only afterwards, in a
	// count the model has no baseline to judge. Requiring an explicit
	// all=true or expect_replacements=N makes a wide edit a decision rather
	// than an accident.
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
			return fmt.Sprintf(readBeforeWriteStaleReadDenial, display, display), taskengine.DataTypeString, nil
		}
	}

	if err := h.writeFileDurable(ctx, absPath, []byte(newContent)); err != nil {
		if isDiskFull(err) {
			return nil, taskengine.DataTypeAny, fatalf("disk full", "local_fs: failed to write file %s", display)
		}
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to write file: %w", err)
	}
	h.invalidateReads(ctx, absPath)

	newBytes := []byte(newContent)
	return FsSedResult{
		Path:         absPath,
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

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

func (h *LocalFSTools) entryFilterFor(ctx context.Context, absRoot string) entryFilter {
	f := entryFilter{
		skipDirs:   h.skipDirNamesFromPolicy(ctx),
		allowExts:  h.listExtensionsFromPolicy(ctx),
		deniedSubs: h.deniedSubstringsFromPolicy(ctx),
	}
	if h.useGitignoreFromPolicy(ctx) {
		if base, err := h.absAllowedDir(ctx); err == nil {
			f.ignore = gitignoreFor(base)
		}
	}
	return f
}

func (h *LocalFSTools) listDir(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, _ := argString(args, "path")
	if path == "" {
		path = "."
	}
	listRootArg := filepath.Clean(path)

	absRoot, display, st, err := h.resolveTarget(ctx, "list_dir", listRootArg)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if !st.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: list_dir: %s is not a directory: %s",
			display, describePathForError(absRoot, st),
		)
	}

	recursive, _ := argBool(args, "recursive")
	policyMaxDepth := h.maxListDepthFromPolicy(ctx)
	reqDepth := 1
	if recursive {
		reqDepth = 3
		if v, ok := argInt(args, "max_depth"); ok && v >= 1 {
			reqDepth = v
		}
		if reqDepth > policyMaxDepth {
			reqDepth = policyMaxDepth
		}
	}

	offset := 0
	if v, ok := argInt(args, "offset"); ok && v > 0 {
		offset = v
	}

	budget, unlimited := h.maxOutputBytesFromPolicy(ctx)
	if unlimited {
		budget = 0
	}
	// Leave headroom for the truncation notice itself, so appending it cannot
	// push the result back over the cap.
	if budget > 1024 {
		budget -= 512
	}

	filter := h.entryFilterFor(ctx, absRoot)
	// The base for relative paths used by gitignore matching is the workspace
	// root, not the listed subdirectory.
	baseRoot, baseErr := h.absAllowedDir(ctx)
	if baseErr != nil {
		baseRoot = absRoot
	}

	c := &listCollector{
		budget:  budget,
		offset:  offset,
		maxScan: h.maxListEntriesScannedFromPolicy(ctx),
	}

	if recursive {
		if err := h.walkListDir(ctx, listRootArg, absRoot, baseRoot, "", 1, reqDepth, filter, c); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	} else {
		// The non-recursive listing returns bare entry names rather than paths
		// relative to the project root — same as it always has, so existing
		// callers and fixtures are unaffected.
		if err := h.listOneLevel(ctx, absRoot, baseRoot, filter, c); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	}

	out := strings.Join(c.out, "\n")
	if c.truncated {
		if out != "" {
			out += "\n"
		}
		out += fmt.Sprintf(
			"local_fs: list_dir truncated — showed %d entries starting at offset %d; output capped at %d bytes. To continue call list_dir with offset: %d (same path, recursive, and max_depth). %s",
			len(c.out), offset, budget, c.nextOffset(), severityRecoverable)
	}
	if len(c.out) == 0 && !c.truncated {
		return "", taskengine.DataTypeString, nil
	}
	return out, taskengine.DataTypeString, nil
}

// listOneLevel renders a single directory level as bare entry names, applying
// the same filters and byte budget as the recursive walk.
func (h *LocalFSTools) listOneLevel(ctx context.Context, absRoot, baseRoot string, filter entryFilter, c *listCollector) error {
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return fmt.Errorf("local_fs: failed to read directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !c.visit() {
			return nil
		}
		abs := filepath.Join(absRoot, e.Name())
		relFromBase := e.Name()
		if r, relErr := filepath.Rel(baseRoot, abs); relErr == nil {
			relFromBase = filepath.ToSlash(r)
		}
		if filter.skip(relFromBase, e.Name(), e.IsDir()) {
			continue
		}
		if e.IsDir() {
			if !c.add(e.Name() + "/") {
				return nil
			}
			continue
		}
		name := e.Name()
		if info, infoErr := e.Info(); infoErr == nil {
			name += fileEntrySuffix(info)
		}
		if !c.add(name) {
			return nil
		}
	}
	return nil
}

// walkListDir appends entries to the collector, one per line, directories
// ending in '/'. Paths are rendered relative to the argument the caller passed
// to list_dir, so they can be fed straight back into another call.
//
// Symlinks are never followed: os.ReadDir reports a symlinked directory with
// IsDir() == false, so the walk cannot leave the tree it started in and no
// per-entry containment re-check is needed. Hoisting that check out of the loop
// removes several syscalls per file from every listing.
func (h *LocalFSTools) walkListDir(
	ctx context.Context,
	listRootArg, curAbs, baseRoot, relFromListRoot string,
	depth, maxDepth int,
	filter entryFilter,
	c *listCollector,
) error {
	entries, err := os.ReadDir(curAbs)
	if err != nil {
		// An unreadable subdirectory should not abort a listing that is
		// otherwise useful; the top-level call already verified the root.
		if depth > 1 {
			return nil
		}
		return fmt.Errorf("local_fs: failed to read directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if !c.visit() {
			return nil
		}

		rel := e.Name()
		if relFromListRoot != "" {
			rel = filepath.ToSlash(filepath.Join(relFromListRoot, e.Name()))
		}

		userPath := rel
		if listRootArg != "" && listRootArg != "." {
			userPath = filepath.ToSlash(filepath.Join(listRootArg, rel))
		}

		childAbs := filepath.Join(curAbs, e.Name())
		relFromBase := userPath
		if r, relErr := filepath.Rel(baseRoot, childAbs); relErr == nil {
			relFromBase = filepath.ToSlash(r)
		}

		if filter.skip(relFromBase, e.Name(), e.IsDir()) {
			continue
		}

		if e.IsDir() {
			if !c.add(userPath + "/") {
				return nil
			}
			if depth >= maxDepth {
				continue
			}
			if err := h.walkListDir(ctx, listRootArg, childAbs, baseRoot, rel, depth+1, maxDepth, filter, c); err != nil {
				return err
			}
			if c.truncated {
				return nil
			}
			continue
		}

		name := userPath
		if info, infoErr := e.Info(); infoErr == nil {
			name += fileEntrySuffix(info)
		}
		if !c.add(name) {
			return nil
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// grep / find_files / count_stats / stat_file
// ---------------------------------------------------------------------------

// grepLineRange returns 1-based inclusive [start, end] line numbers to search
// within numLines total lines.
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

func (h *LocalFSTools) grep(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for grep")
	}
	pattern, ok := argString(args, "pattern")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: pattern required for grep")
	}
	if len(pattern) > 8192 {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: grep: pattern exceeds 8192 characters")
	}

	useRegex, _ := argBool(args, "regex")
	var re *regexp.Regexp
	if useRegex {
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, taskengine.DataTypeAny, recoverablef("local_fs: grep: invalid regex: %v", err)
		}
	}

	absPath, display, info, err := h.resolveTarget(ctx, "grep", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef(
			"local_fs: grep: %s is a directory; grep searches a single file — use find_files to locate candidates first", display)
	}
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.refuseBinary(ctx, "grep", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	start, end := grepLineRange(args, len(lines))
	maxMatches := h.maxGrepMatchesFromPolicy(ctx)
	budget, unlimited := h.maxOutputBytesFromPolicy(ctx)
	if unlimited {
		budget = 0
	}
	if budget > 1024 {
		budget -= 512
	}

	var (
		matches   []string
		size      int64
		truncated bool
		hitCap    bool // stopped on _max_grep_matches rather than the byte budget
		lastLine  = end
	)
	for lineNo := start; lineNo <= end; lineNo++ {
		if lineNo < 1 || lineNo > len(lines) {
			continue
		}
		line := lines[lineNo-1]
		var matched bool
		if useRegex {
			matched = re.MatchString(line)
		} else {
			matched = strings.Contains(line, pattern)
		}
		if !matched {
			continue
		}
		entry := fmt.Sprintf("%d: %s", lineNo, line)
		// Truncate rather than discard.
		//
		// Hitting the cap used to return an error, throwing away every match
		// already found — the model paid for the search and got nothing back,
		// not even the knowledge of where the matches started.
		if budget > 0 && size+int64(len(entry)+1) > budget {
			truncated = true
			lastLine = lineNo - 1
			break
		}
		matches = append(matches, entry)
		size += int64(len(entry) + 1)
		if len(matches) >= maxMatches {
			truncated = true
			hitCap = true
			lastLine = lineNo
			break
		}
	}

	out := strings.Join(matches, "\n")
	if truncated {
		if out != "" {
			out += "\n"
		}
		noun := "matches"
		if len(matches) == 1 {
			noun = "match"
		}
		// Name the policy key that caused the stop, not just the resume line:
		// an operator reading this needs to know which knob to turn, and that
		// pointer is the one thing the previous hard-error version got right.
		reason := fmt.Sprintf("output capped at %d bytes (raise _max_output_bytes or _model_context_tokens in tools_policies.local_fs)", budget)
		if hitCap {
			reason = fmt.Sprintf("hit the %d-match cap (raise _max_grep_matches in tools_policies.local_fs)", maxMatches)
		}
		out += fmt.Sprintf(
			"local_fs: grep truncated — %d %s shown, searched lines %d-%d of %d; %s. Narrow the pattern or continue with start_line: %d. %s",
			len(matches), noun, start, lastLine, len(lines), reason, lastLine+1, severityRecoverable)
	}
	return out, taskengine.DataTypeString, nil
}

// findFiles implements find_files: glob-pattern path discovery under the
// project root. The pattern is matched against the file basename (e.g. "*.go")
// or, when it contains a path separator, against the path relative to the
// search root.
func (h *LocalFSTools) findFiles(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	pattern, ok := argString(args, "pattern")
	if !ok || pattern == "" {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: pattern required for find_files")
	}
	rootArg, _ := argString(args, "path")
	if rootArg == "" {
		rootArg = "."
	}

	absRoot, display, info, err := h.resolveTarget(ctx, "find_files", filepath.Clean(rootArg))
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if !info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: find_files: %s is not a directory", display)
	}

	patternHasSlash := strings.ContainsRune(pattern, '/')
	filter := h.entryFilterFor(ctx, absRoot)
	maxResults := h.maxFindResultsFromPolicy(ctx)
	maxDepth := h.maxFindDepthFromPolicy(ctx)
	baseRoot, baseErr := h.absAllowedDir(ctx)
	if baseErr != nil {
		baseRoot = absRoot
	}

	var matches []string
	truncated := false

	walkErr := filepath.WalkDir(absRoot, func(walkPath string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if truncated {
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(absRoot, walkPath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		relFromBase := rel
		if r, e := filepath.Rel(baseRoot, walkPath); e == nil {
			relFromBase = filepath.ToSlash(r)
		}

		if d.IsDir() {
			// find_files previously ignored _denied_path_substrings entirely
			// and had no depth bound, so it could enumerate paths that
			// read_file would refuse to open.
			if filter.skip(relFromBase, d.Name(), true) {
				return filepath.SkipDir
			}
			if strings.Count(rel, "/")+1 > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if filter.skip(relFromBase, d.Name(), false) {
			return nil
		}

		var matched bool
		if patternHasSlash {
			matched, _ = filepath.Match(pattern, rel)
		} else {
			matched, _ = filepath.Match(pattern, d.Name())
		}
		if matched {
			matches = append(matches, rel)
			if len(matches) >= maxResults {
				truncated = true
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: find_files: %w", walkErr)
	}
	if matches == nil {
		matches = []string{}
	}

	type findResult struct {
		Matches   []string `json:"matches"`
		Count     int      `json:"count"`
		Truncated bool     `json:"truncated,omitempty"`
		Note      string   `json:"note,omitempty"`
	}
	res := findResult{Matches: matches, Count: len(matches), Truncated: truncated}
	if truncated {
		res.Note = fmt.Sprintf("capped at %d results; narrow the pattern or search a subdirectory", maxResults)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: find_files marshal: %w", err)
	}
	s := string(out)
	if err := h.checkToolOutputLimit(ctx, "find_files", s); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return s, taskengine.DataTypeJSON, nil
}

func (h *LocalFSTools) countStats(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for count_stats")
	}

	absPath, display, info, err := h.resolveTarget(ctx, "count_stats", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if info.IsDir() {
		return nil, taskengine.DataTypeAny, recoverablef("local_fs: count_stats: %s is a directory", display)
	}
	if err := h.checkFileSizeLimit(ctx, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if err := h.refuseBinary(ctx, "count_stats", display, absPath); err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	content, err := h.fileIO.ReadFile(ctx, absPath)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: failed to read file: %w", err)
	}

	text := string(content)
	lineCount := countTextLines(text)
	if len(content) > 0 && content[len(content)-1] == '\n' {
		lineCount--
	}
	result := fmt.Sprintf("Lines: %d, Words: %d, Bytes: %d", lineCount, len(strings.Fields(text)), len(content))
	if err := h.checkToolOutputLimit(ctx, "count_stats", result); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return result, taskengine.DataTypeString, nil
}

func (h *LocalFSTools) statFile(ctx context.Context, args map[string]any) (any, taskengine.DataType, error) {
	path, ok := argString(args, "path")
	if !ok {
		return nil, taskengine.DataTypeAny, errors.New("local_fs: path required for stat_file")
	}

	absPath, _, info, err := h.resolveTarget(ctx, "stat_file", path)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	// binary is only meaningful (and only sniffed) for regular files; a
	// directory or other special file is never "binary" in the sense a model
	// asking whether it is safe to read_file cares about.
	binary := false
	if info.Mode().IsRegular() {
		if b, sniffErr := sniffBinaryFile(absPath); sniffErr == nil {
			binary = b
		}
	}

	result := map[string]any{
		"name":       info.Name(),
		"size":       info.Size(),
		"sizeHuman":  humanSize(info.Size()),
		"modTime":    info.ModTime().Format(time.RFC3339),
		"isDir":      info.IsDir(),
		"mode":       info.Mode().String(),
		"executable": isExecutable(info),
		"binary":     binary,
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("local_fs: marshal stat: %w", err)
	}
	out := string(b)
	if err := h.checkToolOutputLimit(ctx, "stat_file", out); err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return out, taskengine.DataTypeJSON, nil
}

var _ taskengine.ToolsRepo = (*LocalFSTools)(nil)
