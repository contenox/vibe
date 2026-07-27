package taskengine

import (
	"context"
	"fmt"
	"maps"

	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
)

type templateVarsKey struct{}

// WithTemplateVars attaches a map of template variables to the context.
// MacroEnv expands {{var:name}} from this map. The engine never reads os.Getenv;
// callers (e.g. Contenox CLI, API) build the map and attach it here.
func WithTemplateVars(ctx context.Context, vars map[string]string) context.Context {
	if vars == nil {
		return ctx
	}
	return context.WithValue(ctx, templateVarsKey{}, vars)
}

// TemplateVarsFromContext returns the template variables map from the context.
// Returns nil if not set; a nil map is safe to read (key lookup returns false).
// MacroEnv will return an error for any {{var:key}} whose key is absent.
func TemplateVarsFromContext(ctx context.Context) (map[string]string, error) {
	v, ok := ctx.Value(templateVarsKey{}).(map[string]string)
	if !ok {
		return nil, fmt.Errorf("template vars not set in context")
	}
	return v, nil
}

// MergeTemplateVars overlays keys onto any template vars already in ctx, then
// attaches the combined map. Use this when a nested step must add
// request_id / previous_output without dropping caller-supplied vars like
// model and provider.
func MergeTemplateVars(ctx context.Context, overlay map[string]string) context.Context {
	base := make(map[string]string)
	if existing, err := TemplateVarsFromContext(ctx); err == nil && existing != nil {
		maps.Copy(base, existing)
	}
	maps.Copy(base, overlay)
	return WithTemplateVars(ctx, base)
}

// WithRequestedContextLength attaches a per-request context window. Task
// execution uses it as the resolver's minimum without replacing the
// chain's token_limit guardrail.
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

// WithRuntimeToolsAllowlist attaches a caller-supplied tools allowlist to ctx that
// is intersected with each task's own allowlist inside resolveToolsNames. A caller
// can only further restrict — never expand — what a chain JSON permits. Grammar
// matches TaskDefinition.Tools: nil/[]/["*"]/exact names/["*","!name"].
//
// Use this when a host must enforce per-call policy (such as disabling
// local_shell for a step) regardless of what the chain JSON declares.
// Absent key means "no runtime restriction" — behavior matches pre-feature code.
func WithRuntimeToolsAllowlist(ctx context.Context, allowlist []string) context.Context {
	return context.WithValue(ctx, runtimeToolsAllowlistKey{}, runtimeToolsAllowlist{list: allowlist})
}

// RuntimeToolsAllowlistFromContext returns (allowlist, true) when an allowlist was
// attached via WithRuntimeToolsAllowlist. The returned slice follows the same
// grammar as TaskDefinition.Tools. Returns (nil, false) when no runtime
// allowlist is attached — callers should treat this as "no restriction".
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

// WithEdgeCounts attaches the in-flight edge traversal counts for the current
// chain run to ctx. SimpleEnv updates this between each task step so handlers
// and step-time macros (e.g. {{edge_count:from->to}}) can read counts that
// reflect the loop iteration they are in, not the chain-start zero.
//
// Layout of the map: keys are "<fromTaskID>-><toTaskID>", values are the number
// of times that edge has been traversed in this run.
func WithEdgeCounts(ctx context.Context, counts map[string]int) context.Context {
	return context.WithValue(ctx, edgeCountsKey{}, counts)
}

// EdgeCountsFromContext returns the edge counts attached via WithEdgeCounts, or
// nil if not set. A nil map is safe to read (lookup returns zero).
func EdgeCountsFromContext(ctx context.Context) map[string]int {
	v, _ := ctx.Value(edgeCountsKey{}).(map[string]int)
	return v
}
