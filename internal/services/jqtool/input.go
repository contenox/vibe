package jqtool

// input.go turns an argument into the value gojq runs against: containment and
// the file read for `path`, decoding and normalization for both sources.
//
// CONTAINMENT IS local_fs's, NOT A SECOND IMPLEMENTATION. baseDir, checkPath and
// displayPath below mirror internal/services/localtools/fs.go one for one,
// including the `_allowed_dir` policy override, and every one of them delegates
// the actual boundary decision to internal/services/vfs — the single
// symlink-escape guard in this process. A path outside the workspace is refused
// with the containment sentinel BEFORE any I/O happens, so an escaping symlink
// is never opened, only rejected.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/vfs"
	"gopkg.in/yaml.v3"
)

// Format names the parser used for an input.
const (
	FormatJSON = "json"
	FormatYAML = "yaml"
)

// inlineSource is what Result.Source says when the document came from the
// `input` argument rather than from a file.
const inlineSource = "(inline input)"

// --- Path handling (mirrors localtools.LocalFSTools) -------------------------

func (h *tools) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, h.name)
}

// baseDir returns the directory `path` arguments are resolved against:
// tools_policies.jq._allowed_dir when the chain declared one, else the directory
// the toolset was constructed with, else the per-call cwd resolver. Identical
// precedence to LocalFSTools.baseDir.
//
// There is deliberately NO fallback to the process working directory. git's
// toolset takes one because it can distinguish "declared boundary" from "found a
// repo"; this tool reads an arbitrary named file, so it follows local_fs and
// gointel instead: with nothing declared, there is no boundary to enforce and
// the read is refused rather than silently scoped to wherever the process
// happens to be.
func (h *tools) baseDir(ctx context.Context) (string, error) {
	if args := h.policyArgs(ctx); len(args) > 0 {
		if policyDir := strings.TrimSpace(args["_allowed_dir"]); policyDir != "" {
			cleaned := filepath.Clean(policyDir)
			if filepath.IsAbs(cleaned) {
				return cleaned, nil
			}
			if h.cwdResolver != nil {
				if cwd := strings.TrimSpace(h.cwdResolver(ctx)); cwd != "" {
					return filepath.Clean(filepath.Join(cwd, cleaned)), nil
				}
			}
			return cleaned, nil
		}
	}
	base := h.allowedDir
	if base == "" && h.cwdResolver != nil {
		if r := strings.TrimSpace(h.cwdResolver(ctx)); r != "" {
			base = filepath.Clean(r)
		}
	}
	if base == "" {
		// FATAL, and it names the two ways a root is supplied. gointel learned
		// this the expensive way: a bare "no allowed directory configured" sends
		// the model hunting for a better path spelling forever, when nothing it
		// can type will help. The `input` argument is named too, because unlike
		// gointel this tool has a source that still works.
		return "", fmt.Errorf("%w: no workspace root is configured for this session, so no file path can be resolved; "+
			"the root comes from the runtime's allowed directory (--local-exec-allowed-dir on the CLI) or from the "+
			"session cwd resolver the composition root supplies. Pass the document inline via the `input` argument "+
			"instead — that source needs no workspace root (fatal: no workspace root)", ErrNoWorkspaceRoot)
	}
	return base, nil
}

// absAllowedDir returns the symlink-resolved base directory for this call.
func (h *tools) absAllowedDir(ctx context.Context) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.ResolveRoot(base)
	if err != nil {
		return "", recoverablef("jq: workspace root %s cannot be resolved: %s", echoArg(base), echoErr(err))
	}
	return resolved, nil
}

// checkPath verifies that a path is within the allowed directory. Containment —
// normalization plus symlink-escape guarding — is vfs's, so there is exactly one
// implementation of it shared with local_fs and the /files browse API. A symlink
// inside the sandbox pointing outside it is caught before any I/O.
func (h *tools) checkPath(ctx context.Context, path string) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.Contain(base, path)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			// ONE typed boundary: a "../..", an absolute path elsewhere and a
			// symlink pointing at /etc are the same refusal, so a caller
			// branching on errors.Is(err, ErrEscapesWorkspace) sees all three.
			return "", wrapRecoverable(ErrEscapesWorkspace,
				"path %s escapes allowed directory %s", echoArg(path), base)
		}
		return "", recoverablef("jq: cannot resolve path %s: %s", echoArg(path), echoErr(err))
	}
	return resolved, nil
}

// displayPath renders an absolute path the way the model is expected to address
// it: relative to the workspace root, forward-slashed. Every model-facing
// message uses it, so a path in an error is a path the model can paste straight
// back into the next call.
func (h *tools) displayPath(ctx context.Context, absPath string) string {
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

// --- Loading -----------------------------------------------------------------

// loaded is a decoded input: the documents to run the filter over, plus what the
// result should say about where they came from.
type loaded struct {
	docs   []any
	source string
	format string
	note   string
}

// loadPath reads and decodes a workspace file.
func (h *tools) loadPath(ctx context.Context, path, format string) (*loaded, error) {
	absPath, err := h.checkPath(ctx, path)
	if err != nil {
		return nil, err
	}
	display := h.displayPath(ctx, absPath)

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, recoverablef("jq: %s does not exist", echoArg(display))
		}
		return nil, recoverablef("jq: cannot stat %s: %s", echoArg(display), echoErr(err))
	}
	if info.IsDir() {
		return nil, recoverablef("jq: %s is a directory, not a document; jq_query reads ONE JSON or YAML file", echoArg(display))
	}
	// Size is checked from the stat, before the read: an 800 MB file costs one
	// syscall to refuse rather than 800 MB of I/O that is then discarded.
	if info.Size() > MaxInputBytes {
		return nil, recoverablef(
			"jq: %s is %d bytes, over the %d-byte input cap; jq_query loads the whole document to query it. "+
				"Narrow the file (jq cannot stream a partial document) or read the region you need with local_fs.read_file_range",
			echoArg(display), info.Size(), MaxInputBytes)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, recoverablef("jq: cannot read %s: %s", echoArg(display), echoErr(err))
	}
	// Re-check after the read: the file may have grown between the stat and the
	// read (a log being appended to is the ordinary case, not an attack).
	if int64(len(data)) > MaxInputBytes {
		return nil, recoverablef("jq: %s grew past the %d-byte input cap while being read", echoArg(display), MaxInputBytes)
	}

	docs, used, cut, err := decode(data, format, formatFromExt(display))
	if err != nil {
		return nil, decorateDecodeError(err, echoArg(display))
	}
	out := &loaded{docs: docs, source: display, format: used}
	if cut {
		out.note = docsTruncatedNote()
	}
	return out, nil
}

// loadInline decodes the `input` argument.
func loadInline(text, format string) (*loaded, error) {
	if len(text) > MaxInputBytes {
		return nil, recoverablef(
			"jq: inline input is %d bytes, over the %d-byte cap. Pasting a large document into an argument spends the "+
				"tokens this tool exists to save — write it to a workspace file and pass `path` instead",
			len(text), MaxInputBytes)
	}
	docs, used, cut, err := decode([]byte(text), format, "")
	if err != nil {
		return nil, decorateDecodeError(err, "the inline `input` argument")
	}
	out := &loaded{docs: docs, source: inlineSource, format: used}
	if cut {
		out.note = docsTruncatedNote()
	}
	return out, nil
}

// loadValue wraps an `input` that arrived as an already-decoded JSON value — a
// model that passes an object or an array rather than a string is being helpful,
// not wrong, and refusing it would cost a turn to teach a distinction that does
// not matter.
func loadValue(v any) (*loaded, error) {
	norm, err := normalize(v, 0)
	if err != nil {
		return nil, err
	}
	return &loaded{docs: []any{norm}, source: inlineSource, format: FormatJSON}, nil
}

// --- Format selection --------------------------------------------------------

// formatFromExt maps a filename to a format, or "" when the extension says
// nothing. .jsonl/.ndjson are JSON: the stream decoder below reads them without
// a special mode.
func formatFromExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json", ".jsonl", ".ndjson":
		return FormatJSON
	case ".yaml", ".yml":
		return FormatYAML
	}
	return ""
}

// candidates returns the parsers to try, in order.
//
// The rule is three-tiered and deterministic, because "guess the format" is
// exactly the kind of cleverness that produces a silent wrong answer:
//
//  1. An explicit `format` argument wins and is the ONLY parser tried, so a
//     failure is reported against the parser the caller asked for.
//  2. Otherwise the file extension decides, and again only that parser is tried.
//     A .json file that does not parse as JSON is a broken .json file, and
//     silently succeeding as YAML would hide that.
//  3. Otherwise — no extension, or an inline string — the first non-space byte
//     picks the first candidate and the other is the fallback. Both are tried
//     because YAML is very nearly a superset of JSON and neither guess is
//     reliable alone; the error names both attempts when both fail.
func candidates(explicit, byExt string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	if byExt != "" {
		return []string{byExt}
	}
	return []string{FormatJSON, FormatYAML}
}

// sniff picks which of the two candidates to try first from the leading bytes.
func sniff(data []byte) string {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return FormatJSON
	}
	switch trimmed[0] {
	case '{', '[', '"':
		return FormatJSON
	}
	if trimmed[0] == '-' || trimmed[0] == '#' || trimmed[0] == '%' {
		// "- item", a comment, or a %YAML directive: none of them are JSON.
		return FormatYAML
	}
	if trimmed[0] >= '0' && trimmed[0] <= '9' {
		return FormatJSON
	}
	return FormatYAML
}

// decodeError carries both attempts so the caller can render a message that
// names what was actually tried.
type decodeError struct {
	tried []string
	err   error
}

func (e *decodeError) Error() string { return e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

func decorateDecodeError(err error, what string) error {
	var de *decodeError
	if !errors.As(err, &de) {
		return err
	}
	tried := strings.Join(de.tried, " then ")
	return recoverablef(
		"jq: %s is not valid %s: %s. This is a problem with the DOCUMENT, not with the filter — "+
			"check the file with local_fs.read_file, or pass format=\"json\"/\"yaml\" if it was parsed as the wrong one",
		what, tried, echoErr(de.err))
}

// decode parses data into one or more documents, returning the format actually
// used and whether the document stream was cut at maxInputDocs.
func decode(data []byte, explicit, byExt string) (docs []any, format string, truncated bool, err error) {
	order := candidates(explicit, byExt)
	if len(order) > 1 {
		if first := sniff(data); first == FormatYAML {
			order = []string{FormatYAML, FormatJSON}
		}
	}
	var firstErr error
	for _, candidate := range order {
		var (
			got []any
			cut bool
			e   error
		)
		switch candidate {
		case FormatJSON:
			got, cut, e = decodeJSON(data)
		case FormatYAML:
			got, cut, e = decodeYAML(data)
		default:
			return nil, "", false, recoverablef("jq: unknown format %s; use \"json\" or \"yaml\"", echoArg(candidate))
		}
		if e == nil {
			return got, candidate, cut, nil
		}
		if firstErr == nil {
			firstErr = e
		}
	}
	return nil, "", false, &decodeError{tried: order, err: firstErr}
}

// docsTruncatedNote is the explicit marker for a document stream cut at the cap.
// Never silent: without it a `.kind` over a 5000-document manifest bundle would
// answer about the first 1000 and read exactly like a complete answer.
func docsTruncatedNote() string {
	return fmt.Sprintf(
		"TRUNCATED: only the first %d documents of the input stream were read; the filter did not see the rest. "+
			"Split the file, or narrow it before querying.", maxInputDocs)
}

// decodeJSON reads a JSON stream: one value, or several concatenated /
// newline-delimited ones (JSON Lines). UseNumber keeps integer precision that a
// float64 would silently lose — gojq accepts json.Number natively, so a 64-bit
// id in a config file survives the round trip.
func decodeJSON(data []byte) ([]any, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var docs []any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false, err
		}
		norm, err := normalize(v, 0)
		if err != nil {
			return nil, false, err
		}
		docs = append(docs, norm)
		if len(docs) >= maxInputDocs {
			// Peek for one more: the cap only gets REPORTED when something was
			// actually left behind, so an input of exactly maxInputDocs is a
			// complete answer and says so.
			var extra any
			if dec.Decode(&extra) == nil {
				return docs, true, nil
			}
			break
		}
	}
	if len(docs) == 0 {
		return nil, false, errors.New("the document is empty")
	}
	return docs, false, nil
}

// decodeYAML reads a YAML stream, one entry per "---" document.
//
// gopkg.in/yaml.v3 is used rather than the go-yaml gojq's own CLI reaches for:
// it is ALREADY a direct dependency of this module (taskengine parses the chain
// DSL with it), so YAML support here adds no dependency at all, and a chain file
// read by jq_query is parsed by exactly the parser that runs it.
func decodeYAML(data []byte) ([]any, bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false, err
		}
		norm, err := normalize(v, 0)
		if err != nil {
			return nil, false, err
		}
		docs = append(docs, norm)
		if len(docs) >= maxInputDocs {
			var extra any
			if dec.Decode(&extra) == nil {
				return docs, true, nil
			}
			break
		}
	}
	if len(docs) == 0 {
		return nil, false, errors.New("the document is empty")
	}
	return docs, false, nil
}

// --- Normalization -----------------------------------------------------------

// normalize converts a decoded value into the types gojq accepts (nil, bool,
// int, float64, *big.Int, json.Number, string, []any, map[string]any).
//
// It exists for YAML, which produces shapes JSON never does: a mapping with
// non-string keys decodes to map[any]any, a timestamp to time.Time, a !!binary
// to []byte, and an integer to int rather than a number token. Handing any of
// those to gojq unconverted produces "invalid type" deep inside a filter, which
// reads like a bug in the filter rather than in the document.
//
// Depth is capped: a hand-built deeply nested document must be a refusal, not a
// stack overflow that takes the process with it.
func normalize(v any, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, recoverablef("jq: the document nests deeper than %d levels, which jq_query will not load", maxValueDepth)
	}
	switch x := v.(type) {
	case nil, bool, string, float64, json.Number, *big.Int:
		return x, nil
	case int:
		return x, nil
	case int8:
		return int(x), nil
	case int16:
		return int(x), nil
	case int32:
		return int(x), nil
	case int64:
		if int64(int(x)) == x {
			return int(x), nil
		}
		return big.NewInt(x), nil
	case uint:
		return new(big.Int).SetUint64(uint64(x)), nil
	case uint8:
		return int(x), nil
	case uint16:
		return int(x), nil
	case uint32:
		return int(x), nil
	case uint64:
		return new(big.Int).SetUint64(x), nil
	case float32:
		return float64(x), nil
	case []byte:
		// YAML !!binary. Rendered as the source text rather than as a byte
		// array: jq has no byte type and an array of 4000 integers is not what
		// anybody asked for.
		return string(x), nil
	case time.Time:
		// YAML timestamps. RFC3339 is the form every jq date builtin
		// (fromdate, dateadd) accepts, so the value stays usable.
		return x.UTC().Format(time.RFC3339Nano), nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			n, err := normalize(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			n, err := normalize(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			n, err := normalize(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[keyString(k)] = n
		}
		return out, nil
	}
	// Anything else a decoder can produce is rendered rather than dropped: a
	// value the filter can see and reject beats a value that silently vanished.
	return fmt.Sprintf("%v", v), nil
}

// keyString renders a non-string YAML mapping key. JSON object keys are strings,
// so a `1: value` or `true: value` mapping becomes "1" / "true" — the same thing
// every YAML-to-JSON converter does, and the only shape a jq filter can address.
func keyString(k any) string {
	switch key := k.(type) {
	case string:
		return key
	case nil:
		return "null"
	case time.Time:
		return key.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", key)
	}
}
