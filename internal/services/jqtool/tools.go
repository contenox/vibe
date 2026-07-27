package jqtool

// tools.go is the taskengine.ToolsRepo surface.
//
// Dispatch and argument decoding follow internal/services/gointel/tools.go,
// which follows localtools.LocalFSTools.execDispatch: accept arguments from the
// chain input map or from the declarative ToolsCall.Args, reject unknown
// argument NAMES, then hand off to a typed handler. Argument VALUES are coerced
// generously (a model routinely emits {"max": "20"}) while argument NAMES stay
// strict — a silently dropped argument answers a DIFFERENT question than the one
// asked, which is worse than a refusal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// tools implements taskengine.ToolsRepo. The fields mirror
// localtools.GitTools: a constructor-supplied allowed directory, the toolset
// name policy rules address it by, and the per-call cwd resolver surfaces whose
// workspace is a property of the session rather than of the process.
type tools struct {
	allowedDir  string
	name        string
	cwdResolver func(context.Context) string
}

// NewTools creates the jq toolset scoped to allowedDir, the same way
// localtools.NewGitTools takes its directory. An empty allowedDir means no
// declared boundary: `path` arguments are refused (with a message naming how a
// root is supplied) and inline `input` still works.
func NewTools(allowedDir string) taskengine.ToolsRepo {
	return NewToolsWith(allowedDir, ToolsProviderName, nil)
}

// NewToolsWith is NewTools with the toolset name and a per-call working
// directory resolver, mirroring NewLocalFSToolsWith and NewGitToolsWith for
// surfaces (ACP) whose cwd belongs to the session.
func NewToolsWith(allowedDir, name string, cwdResolver func(context.Context) string) taskengine.ToolsRepo {
	cleaned := allowedDir
	if cleaned != "" {
		cleaned = filepath.Clean(cleaned)
	}
	if name == "" {
		name = ToolsProviderName
	}
	return &tools{allowedDir: cleaned, name: name, cwdResolver: cwdResolver}
}

func (h *tools) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, errors.New("jq: tools required")
	}
	args, err := callArgs(input, call)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	toolName := call.ToolName
	if toolName == "" {
		toolName = call.Name
	}

	switch toolName {
	case ToolQuery:
		if err := rejectUnknownArgs(ToolQuery, args, "filter", "path", "input", "format", "max", "deadline_ms"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		res, err := h.query(ctx, args)
		if err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return res, taskengine.DataTypeJSON, nil

	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("jq: unknown tool %q; this toolset provides %s %s",
			echoName(toolName), strings.Join(toolNames, ", "), severityRecoverable)
	}
}

// query resolves the one input source, runs the filter, and returns the payload.
func (h *tools) query(ctx context.Context, args map[string]any) (*Result, error) {
	format, err := normalizeFormat(argString(args, "format"))
	if err != nil {
		return nil, err
	}

	in, err := h.resolveInput(ctx, args, format)
	if err != nil {
		return nil, err
	}

	max, hasMax := argInt(args, "max")
	ms, hasMS := argInt(args, "deadline_ms")
	return execute(ctx, request{
		filter:     argRaw(args, "filter"),
		in:         in,
		maxResults: clampResults(max, hasMax),
		deadline:   clampDeadline(ms, hasMS),
	})
}

// resolveInput enforces the EXACTLY-ONE-SOURCE rule.
//
// It is a refusal rather than a precedence rule ("path wins") on purpose: a call
// carrying both a path and an inline document is a model that changed its mind
// halfway through composing the call, and silently querying one of the two
// produces an answer about a document nobody asked about — the exact failure
// mode a tool result cannot self-correct, because it looks like a success.
func (h *tools) resolveInput(ctx context.Context, args map[string]any, format string) (*loaded, error) {
	path := argString(args, "path")
	raw, hasInput := args["input"]
	if hasInput {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) == "" {
			hasInput = false
		} else if raw == nil {
			hasInput = false
		}
	}

	switch {
	case path != "" && hasInput:
		return nil, recoverablef(
			"jq: pass EITHER path OR input, not both — %s names a file and `input` carries a document, and jq_query "+
				"queries exactly one. Drop whichever one is not the document you meant",
			echoArg(path))

	case path == "" && !hasInput:
		return nil, recoverablef(
			"jq: an input source is required — pass `path` (a file in the workspace, e.g. \"chain-acp.json\") " +
				"or `input` (the document itself, as a JSON/YAML string)")

	case path != "":
		return h.loadPath(ctx, path, format)
	}

	// Inline. A model that passes an object or array rather than a string is
	// being helpful; take it as the value it already is.
	if s, ok := raw.(string); ok {
		return loadInline(s, format)
	}
	return loadValue(raw)
}

// normalizeFormat validates an explicit format argument. Empty means "decide it
// from the extension, then from the content" (see input.go's candidates).
func normalizeFormat(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "":
		return "", nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatYAML, "yml":
		return FormatYAML, nil
	default:
		return "", recoverablef("jq: unknown format %s; use \"json\" or \"yaml\", or omit it and the format is taken from the file extension, then from the content", echoArg(format))
	}
}

func (h *tools) Supports(context.Context) ([]string, error) {
	return append([]string{ToolsProviderName}, toolNames...), nil
}

// GetSchemasForSupportedTools returns no OpenAPI documents: jq is a local
// toolset with a hand-written function schema, exactly like local_fs, gointel,
// workspace and goja. The model-facing contract is GetToolsForToolsByName.
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// ---------------------------------------------------------------------------
// Argument decoding (mirrors gointel/tools.go)
// ---------------------------------------------------------------------------

// callArgs assembles the argument map from the chain input or, for declarative
// `tools` tasks that carry arguments on the call itself, from ToolsCall.Args.
func callArgs(input any, call *taskengine.ToolsCall) (map[string]any, error) {
	if m, ok := input.(map[string]any); ok && len(m) > 0 {
		return m, nil
	}
	if len(call.Args) > 0 {
		out := make(map[string]any, len(call.Args))
		for k, v := range call.Args {
			out[k] = v
		}
		return out, nil
	}
	if m, ok := input.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

func rejectUnknownArgs(toolName string, args map[string]any, allowed ...string) error {
	if len(args) == 0 {
		return nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	var unknown []string
	for key := range args {
		if _, ok := allowedSet[key]; !ok {
			// The KEY is model-supplied too, so it is clamped like every other
			// echoed argument — an unknown-argument error must not be a channel
			// for a megabyte of model-chosen text.
			unknown = append(unknown, echoName(key))
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	sort.Strings(allowed)
	return fmt.Errorf("%s: unknown argument(s): %s (allowed: %s) %s",
		toolName, strings.Join(unknown, ", "), strings.Join(allowed, ", "), severityRecoverable)
}

// argRaw returns a string argument WITHOUT trimming. Used for `filter` only:
// jq is whitespace-insensitive at the edges, but trimming a model-supplied
// program before echoing it back in an error would report a program that is not
// quite the one that was sent.
func argRaw(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return argString(args, key)
}

func argString(args map[string]any, key string) string {
	x, ok := args[key]
	if !ok || x == nil {
		return ""
	}
	switch v := x.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func argInt(args map[string]any, key string) (int, bool) {
	x, ok := args[key]
	if !ok || x == nil {
		return 0, false
	}
	switch v := x.(type) {
	case float64:
		return intFromFloat(v)
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return intFromFloat(f)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
		// A model that writes "20.0" means twenty.
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return intFromFloat(f)
		}
	}
	return 0, false
}

var _ taskengine.ToolsRepo = (*tools)(nil)
