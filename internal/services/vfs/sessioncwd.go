package vfs

import "context"

type sessionCwdContextKey struct{}

// WithSessionCwd stamps the session's own workspace root onto ctx: the absolute
// directory the session's relative tool paths resolve against.
func WithSessionCwd(ctx context.Context, cwd string) context.Context {
	if cwd == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCwdContextKey{}, cwd)
}

// SessionCwdFromContext returns the session's own workspace root stamped by
// WithSessionCwd, or "" when none was stamped.
func SessionCwdFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionCwdContextKey{}).(string)
	return v
}
