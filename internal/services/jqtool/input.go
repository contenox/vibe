package jqtool

// input.go turns an argument into the value gojq runs against: containment
// and the file read for `path`, decoding and normalization for both sources.
//
// Containment mirrors internal/services/localtools/fs.go and delegates the
// boundary decision to internal/services/vfs, so a path outside the
// workspace is refused before any I/O happens.

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

func (h *tools) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, h.name)
}

// baseDir returns the directory `path` arguments are resolved against:
// tools_policies.jq._allowed_dir when declared, else the toolset's configured
// directory, else the per-call cwd resolver. With nothing declared there is
// no boundary to enforce, so the read is refused rather than silently scoped
// to the process's working directory.
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

// checkPath verifies that a path is within the allowed directory via vfs, the
// same containment shared with local_fs and the /files browse API.
func (h *tools) checkPath(ctx context.Context, path string) (string, error) {
	base, err := h.baseDir(ctx)
	if err != nil {
		return "", err
	}
	resolved, err := vfs.Contain(base, path)
	if err != nil {
		if errors.Is(err, vfs.ErrEscape) {
			return "", wrapRecoverable(ErrEscapesWorkspace,
				"path %s escapes allowed directory %s", echoArg(path), base)
		}
		return "", recoverablef("jq: cannot resolve path %s: %s", echoArg(path), echoErr(err))
	}
	return resolved, nil
}

// displayPath renders an absolute path relative to the workspace root,
// forward-slashed, so it can be pasted straight back into the next call.
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

// loaded is a decoded input: the documents to run the filter over, plus what
// the result should say about where they came from.
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
	// Checked from the stat, before the read, so an oversized file costs one
	// syscall to refuse rather than a full discarded read.
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
	// Re-checked after the read: the file may have grown between stat and read.
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

// loadValue wraps an `input` that arrived as an already-decoded JSON value.
func loadValue(v any) (*loaded, error) {
	norm, err := normalize(v, 0)
	if err != nil {
		return nil, err
	}
	return &loaded{docs: []any{norm}, source: inlineSource, format: FormatJSON}, nil
}

// formatFromExt maps a filename to a format, or "" when the extension says
// nothing. .jsonl/.ndjson are JSON: the stream decoder reads them as-is.
func formatFromExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json", ".jsonl", ".ndjson":
		return FormatJSON
	case ".yaml", ".yml":
		return FormatYAML
	}
	return ""
}

// candidates returns the parsers to try, in order: an explicit `format` or a
// file extension is tried alone; otherwise both JSON and YAML are tried,
// leading-byte-sniffed order first, since neither guess is reliable alone.
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
		return FormatYAML
	}
	if trimmed[0] >= '0' && trimmed[0] <= '9' {
		return FormatJSON
	}
	return FormatYAML
}

// decodeError carries both attempts so the caller can name what was tried.
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

// decode parses data into one or more documents, returning the format
// actually used and whether the document stream was cut at maxInputDocs.
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

// docsTruncatedNote is the explicit marker for a document stream cut at the
// cap; never silent, so a partial answer never reads as a complete one.
func docsTruncatedNote() string {
	return fmt.Sprintf(
		"TRUNCATED: only the first %d documents of the input stream were read; the filter did not see the rest. "+
			"Split the file, or narrow it before querying.", maxInputDocs)
}

// decodeJSON reads a JSON stream: one value, or several concatenated /
// newline-delimited ones (JSON Lines). UseNumber preserves integer precision
// a float64 would lose.
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
			// The cap is only reported when something was actually left
			// behind: peek for one more document first.
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

// normalize converts a decoded value into the types gojq accepts (nil, bool,
// int, float64, *big.Int, json.Number, string, []any, map[string]any). YAML
// produces shapes JSON never does (non-string map keys, time.Time, !!binary),
// which this maps into gojq-safe values instead of a downstream "invalid
// type". Depth is capped so a deeply nested document is a refusal, not a
// stack overflow.
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
		// YAML !!binary, rendered as source text: jq has no byte type.
		return string(x), nil
	case time.Time:
		// YAML timestamps, in the RFC3339 form jq's date builtins accept.
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
	// Anything else a decoder can produce is rendered rather than dropped.
	return fmt.Sprintf("%v", v), nil
}

// keyString renders a non-string YAML mapping key the way any YAML-to-JSON
// converter would (`1: value` becomes "1"), the only shape jq can address.
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
