package jqtool

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

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/vfs"
	"gopkg.in/yaml.v3"
)

// Format names the parser used for an input.
const (
	FormatJSON = "json"
	FormatYAML = "yaml"
)

const inlineSource = "(inline input)"

// policyAllowedDir is the [tools_policies.native-jq] key that re-scopes `path`.
const policyAllowedDir = "_allowed_dir"

func (h *tools) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, h.name)
}

func (h *tools) baseDir(ctx context.Context) (string, error) {
	if args := h.policyArgs(ctx); len(args) > 0 {
		if policyDir := strings.TrimSpace(args[policyAllowedDir]); policyDir != "" {
			cleaned := filepath.Clean(policyDir)
			if filepath.IsAbs(cleaned) {
				return cleaned, nil
			}
			if cwd := h.resolveCwd(ctx); cwd != "" {
				return filepath.Clean(filepath.Join(cwd, cleaned)), nil
			}
			return "", fmt.Errorf("%w: tools_policies.%s.%s %q is relative but no session workspace root could be "+
				"resolved to anchor it; set %s to an absolute path, or resume from a process that can restore "+
				"the session's workspace root (fatal: no workspace root)",
				ErrNoWorkspaceRoot, h.name, policyAllowedDir, policyDir, policyAllowedDir)
		}
	}
	base := h.allowedDir
	if base == "" {
		base = h.resolveCwd(ctx)
	}
	if base == "" {
		return "", fmt.Errorf("%w: no workspace root is configured for this session, so no file path can be resolved; "+
			"the root comes from the runtime's allowed directory (--local-exec-allowed-dir on the CLI) or from the "+
			"session cwd resolver the composition root supplies. Pass the document inline via the `input` argument "+
			"instead — that source needs no workspace root (fatal: no workspace root)", ErrNoWorkspaceRoot)
	}
	return base, nil
}

func (h *tools) resolveCwd(ctx context.Context) string {
	if root := vfs.SessionCwdFromContext(ctx); root != "" {
		return filepath.Clean(root)
	}
	if h.cwdResolver != nil {
		if r := strings.TrimSpace(h.cwdResolver(ctx)); r != "" {
			return filepath.Clean(r)
		}
	}
	return ""
}

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

type loaded struct {
	docs   []any
	source string
	format string
	note   string
}

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
	// Checked before the read: an oversized file costs one stat, not a discarded read.
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

func loadValue(v any) (*loaded, error) {
	norm, err := normalize(v, 0)
	if err != nil {
		return nil, err
	}
	return &loaded{docs: []any{norm}, source: inlineSource, format: FormatJSON}, nil
}

func formatFromExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json", ".jsonl", ".ndjson":
		return FormatJSON
	case ".yaml", ".yml":
		return FormatYAML
	}
	return ""
}

func candidates(explicit, byExt string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	if byExt != "" {
		return []string{byExt}
	}
	return []string{FormatJSON, FormatYAML}
}

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

func docsTruncatedNote() string {
	return fmt.Sprintf(
		"TRUNCATED: only the first %d documents of the input stream were read; the filter did not see the rest. "+
			"Split the file, or narrow it before querying.", maxInputDocs)
}

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
	return fmt.Sprintf("%v", v), nil
}

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
