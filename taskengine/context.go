package taskengine

import "context"

type templateVarsKey struct{}

// WithTemplateVars attaches a map of template variables to the context.
// MacroEnv expands {{var:name}} from this map. The engine never reads os.Getenv;
// callers (e.g. vibe, API) build the map and attach it here.
func WithTemplateVars(ctx context.Context, vars map[string]string) context.Context {
	if vars == nil {
		return ctx
	}
	return context.WithValue(ctx, templateVarsKey{}, vars)
}

// TemplateVarsFromContext returns the template variables map from the context.
// Returns nil if not set; MacroEnv treats nil as empty (missing var:* replaced with empty string).
func TemplateVarsFromContext(ctx context.Context) map[string]string {
	v, _ := ctx.Value(templateVarsKey{}).(map[string]string)
	return v
}
