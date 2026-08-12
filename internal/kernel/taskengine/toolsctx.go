package taskengine

import "context"

type toolsArgsKey struct{ toolsName string }

// WithToolsArgs stores an immutable copy of args for the named tools in ctx.
func WithToolsArgs(ctx context.Context, toolsName string, args map[string]string) context.Context {
	if len(args) == 0 {
		return ctx
	}
	cp := make(map[string]string, len(args))
	for k, v := range args {
		cp[k] = v
	}
	return context.WithValue(ctx, toolsArgsKey{toolsName}, cp)
}

// ToolsArgsFromContext returns the args stored for toolsName, or nil if none were set; the returned map must not be mutated.
func ToolsArgsFromContext(ctx context.Context, toolsName string) map[string]string {
	m, _ := ctx.Value(toolsArgsKey{toolsName}).(map[string]string)
	return m
}
