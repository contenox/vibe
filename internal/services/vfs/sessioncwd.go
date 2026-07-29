package vfs

// sessioncwd.go carries the one piece of session identity a resumed run must
// not lose: the directory a session's tools resolve relative paths against.
//
// A live session (e.g. an ACP connection) knows its own cwd from its
// transport. A run that parks on a human approval and resumes later — in any
// process, on any machine, from any working directory — has no transport to
// ask. Whoever launches a run stamps this value onto ctx once, up front; the
// checkpoint/resume path (agentservice) persists it and restores it onto the
// resumed run's ctx before execution, so a local_fs/git/jq call made after
// resume resolves "." exactly as the original call would have, never against
// the resuming process's own working directory.

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
// WithSessionCwd, or "" when none was stamped (a live session whose caller
// predates this plumbing, or a resumed run whose checkpoint predates it).
//
// Tool baseDir resolution should prefer this over any live cwd resolver: it
// is what the *session* itself established, immutable across whichever
// process happens to be resuming it — a live per-process resolver reflects
// only the resumer's own vantage point.
func SessionCwdFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionCwdContextKey{}).(string)
	return v
}
