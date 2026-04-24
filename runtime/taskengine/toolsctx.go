package taskengine

import "context"

// toolsArgsKey is an unexported typed key to prevent context value collisions
// across packages. One key instance is created per tools name.
type toolsArgsKey struct{ toolsName string }

// WithToolsArgs stores a copy of args for the named tools in ctx.
//
// The map is copied on entry so the stored value is immutable — callers must
// not rely on mutating the original map being visible to tools implementations.
// This ensures no data races when the same context is read concurrently
// (e.g. during tool-list construction in ExecEnv).
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

// ToolsArgsFromContext returns the args previously stored for toolsName, or nil
// if none were set. The returned map must not be mutated by the caller.
func ToolsArgsFromContext(ctx context.Context, toolsName string) map[string]string {
	m, _ := ctx.Value(toolsArgsKey{toolsName}).(map[string]string)
	return m
}
