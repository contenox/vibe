package gointel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// Tool names. The provider ("gointel") exposes six function tools, each a pure read of an in-memory type-checked snapshot: no process spawned, no write, nothing leaves the workspace. Expected to sit at allow tier.
const (
	ToolDescribe        = "go_describe"
	ToolDefinition      = "go_definition"
	ToolReferences      = "go_references"
	ToolImplementations = "go_implementations"
	ToolSymbols         = "go_symbols"
	ToolDiagnostics     = "go_diagnostics"
)

// toolNames is the declaration order used by Supports and the tool list.
var toolNames = []string{
	ToolDescribe,
	ToolDefinition,
	ToolReferences,
	ToolImplementations,
	ToolSymbols,
	ToolDiagnostics,
}

// tools implements taskengine.ToolsRepo over an Index. Dispatch accepts args from the chain input map or from the declarative ToolsCall.Args, rejects unknown argument names per tool, then hands off to a typed handler.
type tools struct {
	ix Index
}

// NewTools returns the gointel ToolsRepo. Register it in the engine's local tools map under ToolsProviderName so it is HITL-wrapped like every other toolset.
func NewTools(ix Index) taskengine.ToolsRepo {
	return &tools{ix: ix}
}

func (h *tools) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, errors.New("gointel: tools required")
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
	case ToolDescribe:
		if err := rejectUnknownArgs(ToolDescribe, args, "symbol", "dir"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return jsonResult(h.ix.Describe(ctx, Request{Dir: argString(args, "dir"), Symbol: argString(args, "symbol")}))

	case ToolDefinition:
		if err := rejectUnknownArgs(ToolDefinition, args, "symbol", "dir"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return jsonResult(h.ix.Definition(ctx, Request{Dir: argString(args, "dir"), Symbol: argString(args, "symbol")}))

	case ToolReferences:
		if err := rejectUnknownArgs(ToolReferences, args, "symbol", "dir", "max"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		max, _ := argInt(args, "max")
		return jsonResult(h.ix.References(ctx, Request{Dir: argString(args, "dir"), Symbol: argString(args, "symbol"), Max: max}))

	case ToolImplementations:
		if err := rejectUnknownArgs(ToolImplementations, args, "symbol", "dir"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		return jsonResult(h.ix.Implementations(ctx, Request{Dir: argString(args, "dir"), Symbol: argString(args, "symbol")}))

	case ToolSymbols:
		if err := rejectUnknownArgs(ToolSymbols, args, "target", "dir", "max"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		max, _ := argInt(args, "max")
		return jsonResult(h.ix.Symbols(ctx, Request{Dir: argString(args, "dir"), Target: argString(args, "target"), Max: max}))

	case ToolDiagnostics:
		if err := rejectUnknownArgs(ToolDiagnostics, args, "scope", "target", "dir", "max", "passes"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		max, _ := argInt(args, "max")
		return jsonResult(h.ix.Diagnostics(ctx, Request{
			Dir:    argString(args, "dir"),
			Target: argString(args, "target"),
			Scope:  argString(args, "scope"),
			Passes: argStrings(args, "passes"),
			Max:    max,
		}))

	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("gointel: unknown tool %q; this toolset provides %s %s",
			toolName, strings.Join(toolNames, ", "), severityRecoverable)
	}
}

// jsonResult adapts a typed query result to the engine's (any, DataType, error) shape, so the payload the model sees is exactly the declared schema.
func jsonResult[T any](res *T, err error) (any, taskengine.DataType, error) {
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return res, taskengine.DataTypeJSON, nil
}

func (h *tools) Supports(context.Context) ([]string, error) {
	return append([]string{ToolsProviderName}, toolNames...), nil
}

// GetSchemasForSupportedTools returns no OpenAPI documents: gointel is a local toolset with hand-written function schemas; the model-facing contract is GetToolsForToolsByName.
func (h *tools) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// Argument decoding: small models routinely emit JSON scalars as strings ({"max": "20"}), and a strict type assertion would silently answer a different question than the one asked. Argument names stay strict — rejectUnknownArgs is the guard.

// callArgs assembles the argument map from the chain input or, for declarative `tools` tasks that carry arguments on the call itself, from ToolsCall.Args.
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
			// The key is model-supplied too, so it is clamped like every other echoed argument.
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

// argStrings accepts a JSON array, a Go []string, or a comma-separated string — the three shapes a model actually emits for a list argument.
func argStrings(args map[string]any, key string) []string {
	x, ok := args[key]
	if !ok || x == nil {
		return nil
	}
	split := func(s string) []string {
		var out []string
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	switch v := x.(type) {
	case string:
		return split(v)
	case []string:
		return v
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				if p := strings.TrimSpace(s); p != "" {
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
}

// intFromFloat converts a JSON number to an int without Go's undefined float→int behavior outside the integer range. Out-of-range saturates; NaN reads as "no value" (documented default). Callers clamp again to their own ceiling.
func intFromFloat(f float64) (int, bool) {
	switch {
	case f != f: // NaN
		return 0, false
	case f >= float64(maxInt):
		return maxInt, true
	case f <= float64(minInt):
		return minInt, true
	}
	return int(f), true
}

const (
	maxInt = int(^uint(0) >> 1)
	minInt = -maxInt - 1
)

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
	}
	return 0, false
}

var _ taskengine.ToolsRepo = (*tools)(nil)
