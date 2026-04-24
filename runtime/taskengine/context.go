package taskengine

import (
	"context"
	"fmt"
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
// attaches the combined map. Use this when a nested step (e.g. plan execution)
// must add request_id / previous_output without dropping caller-supplied vars
// like model and provider.
func MergeTemplateVars(ctx context.Context, overlay map[string]string) context.Context {
	base := make(map[string]string)
	if existing, err := TemplateVarsFromContext(ctx); err == nil && existing != nil {
		for k, v := range existing {
			base[k] = v
		}
	}
	if overlay != nil {
		for k, v := range overlay {
			base[k] = v
		}
	}
	return WithTemplateVars(ctx, base)
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
// Use this when a host (e.g. planservice) must enforce per-call policy (such as
// disabling local_shell for a step) regardless of what the chain JSON declares.
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

type planStepContextKey struct{}

type planStepContext struct {
	planID string
	stepID string
}

// WithPlanStepContext attaches plan + step identity to ctx. Tools compiled into
// a plan's per-step DAG (e.g. plan_summary persist/fallback) read this to know
// which DB row to write, since the identity is chosen at ClaimNextPendingStep
// time — not at plancompile time — and cannot live in the compiled chain JSON.
// Mirrors the WithTemplateVars / WithRuntimeToolsAllowlist pattern: unexported
// key struct, wrapper value, ok-convention getter.
func WithPlanStepContext(ctx context.Context, planID, stepID string) context.Context {
	return context.WithValue(ctx, planStepContextKey{}, planStepContext{planID: planID, stepID: stepID})
}

// PlanStepContextFromContext returns (planID, stepID, true) when attached.
// Returns ("", "", false) when not set — callers should fail cleanly in that case
// rather than write to an unknown row.
func PlanStepContextFromContext(ctx context.Context) (string, string, bool) {
	v, ok := ctx.Value(planStepContextKey{}).(planStepContext)
	if !ok {
		return "", "", false
	}
	return v.planID, v.stepID, true
}
