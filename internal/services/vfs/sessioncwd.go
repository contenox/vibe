package vfs

import "context"

type sessionCwdContextKey struct{}

// WithSessionCwd stamps the session's own workspace root onto ctx. cwd should
// be the absolute directory the session's relative tool paths are meant to
// resolve against — e.g. the ACP session's negotiated cwd, or the CLI
// process's cwd at the moment a run was started.
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
