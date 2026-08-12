package taskengine

import (
	"context"
	"fmt"
	"maps"

	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

type templateVarsKey struct{}

// WithTemplateVars attaches a map of template variables that MacroEnv expands {{var:name}} from.
func WithTemplateVars(ctx context.Context, vars map[string]string) context.Context {
	if vars == nil {
		return ctx
	}
	return context.WithValue(ctx, templateVarsKey{}, vars)
}

// TemplateVarsFromContext returns the template variables attached via WithTemplateVars, or an error if none were set.
func TemplateVarsFromContext(ctx context.Context) (map[string]string, error) {
	v, ok := ctx.Value(templateVarsKey{}).(map[string]string)
	if !ok {
		return nil, fmt.Errorf("template vars not set in context")
	}
	return v, nil
}

// MergeTemplateVars overlays keys onto any template vars already in ctx and reattaches the combined map.
func MergeTemplateVars(ctx context.Context, overlay map[string]string) context.Context {
	base := make(map[string]string)
	if existing, err := TemplateVarsFromContext(ctx); err == nil && existing != nil {
		maps.Copy(base, existing)
	}
	maps.Copy(base, overlay)
	return WithTemplateVars(ctx, base)
}

// WithRequestedContextLength attaches a per-request context window used as the resolver's minimum, without replacing the chain's token_limit guardrail.
func WithRequestedContextLength(ctx context.Context, contextLength int) context.Context {
	return libmodelprovider.WithRequestedContextLength(ctx, contextLength)
}

// RequestedContextLengthFromContext returns the positive context window attached
// by WithRequestedContextLength, or 0 when the caller did not request one.
func RequestedContextLengthFromContext(ctx context.Context) int {
	return libmodelprovider.RequestedContextLengthFromContext(ctx)
}

type runtimeToolsAllowlistKey struct{}

type runtimeToolsAllowlist struct {
	list []string
}

// WithRuntimeToolsAllowlist attaches a caller-supplied tools allowlist to ctx that is intersected with (never expands) each task's own allowlist.
func WithRuntimeToolsAllowlist(ctx context.Context, allowlist []string) context.Context {
	return context.WithValue(ctx, runtimeToolsAllowlistKey{}, runtimeToolsAllowlist{list: allowlist})
}

// RuntimeToolsAllowlistFromContext returns the allowlist attached via WithRuntimeToolsAllowlist, or (nil, false) if none was attached.
func RuntimeToolsAllowlistFromContext(ctx context.Context) ([]string, bool) {
	v, ok := ctx.Value(runtimeToolsAllowlistKey{}).(runtimeToolsAllowlist)
	if !ok {
		return nil, false
	}
	return v.list, true
}

type contextKey string

const (
	ContextKeyOutputByteLimit contextKey = "output_byte_limit"
	ContextKeyToolCallID      contextKey = "tool_call_id"
)

type edgeCountsKey struct{}

// WithEdgeCounts attaches the in-flight edge traversal counts (keyed "<fromTaskID>-><toTaskID>") for the current chain run to ctx.
func WithEdgeCounts(ctx context.Context, counts map[string]int) context.Context {
	return context.WithValue(ctx, edgeCountsKey{}, counts)
}

// EdgeCountsFromContext returns the edge counts attached via WithEdgeCounts, or nil if not set.
func EdgeCountsFromContext(ctx context.Context) map[string]int {
	v, _ := ctx.Value(edgeCountsKey{}).(map[string]int)
	return v
}
