package taskengine

import "context"

// hookArgsKey is an unexported typed key to prevent context value collisions
// across packages. One key instance is created per hook name.
type hookArgsKey struct{ hookName string }

// WithHookArgs stores a copy of args for the named hook in ctx.
//
// The map is copied on entry so the stored value is immutable — callers must
// not rely on mutating the original map being visible to hook implementations.
// This ensures no data races when the same context is read concurrently
// (e.g. during tool-list construction in ExecEnv).
func WithHookArgs(ctx context.Context, hookName string, args map[string]string) context.Context {
	if len(args) == 0 {
		return ctx
	}
	cp := make(map[string]string, len(args))
	for k, v := range args {
		cp[k] = v
	}
	return context.WithValue(ctx, hookArgsKey{hookName}, cp)
}

// HookArgsFromContext returns the args previously stored for hookName, or nil
// if none were set. The returned map must not be mutated by the caller.
func HookArgsFromContext(ctx context.Context, hookName string) map[string]string {
	m, _ := ctx.Value(hookArgsKey{hookName}).(map[string]string)
	return m
}
