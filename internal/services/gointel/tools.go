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
)

// Tool names the gointel provider exposes; each is a pure read of an in-memory type-checked snapshot at allow tier.
const (
	ToolDescribe        = "go_describe"
	ToolDefinition      = "go_definition"
	ToolReferences      = "go_references"
	ToolImplementations = "go_implementations"
	ToolSymbols         = "go_symbols"
	ToolDiagnostics     = "go_diagnostics"
)

var toolNames = []string{
	ToolDescribe,
	ToolDefinition,
	ToolReferences,
	ToolImplementations,
	ToolSymbols,
	ToolDiagnostics,
}

type tools struct {
	ix Index
}

// NewTools returns the gointel ToolsRepo; register it under ToolsProviderName in the engine's local tools map.
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
	lim := limitsFrom(ctx, ToolsProviderName)

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
		max = clampMax(max, lim.references, defaultRefCap)
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
		max = clampMax(max, lim.symbols, defaultSymbolCap)
		return jsonResult(h.ix.Symbols(ctx, Request{Dir: argString(args, "dir"), Target: argString(args, "target"), Max: max}))

	case ToolDiagnostics:
		if err := rejectUnknownArgs(ToolDiagnostics, args, "scope", "target", "dir", "max", "passes"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
		max, _ := argInt(args, "max")
		max = clampMax(max, lim.diagnostics, defaultDiagCap)
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

func jsonResult[T any](res *T, err error) (any, taskengine.DataType, error) {
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	return res, taskengine.DataTypeJSON, nil
}

// Supports reports the scoped toolset name alone: the un-prefixed tool names are not allowlist entries, so an entry addresses the whole set and "!native-go" removes every tool with it; the engine expands the toolset through GetToolsForToolsByName.
func (h *tools) Supports(context.Context) ([]string, error) {
	return []string{ToolsProviderName}, nil
}

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
