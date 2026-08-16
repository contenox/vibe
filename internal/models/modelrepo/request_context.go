package modelrepo

import "context"

type requestedContextLengthKey struct{}

// WithRequestedContextLength attaches a per-request context window to ctx.
// Providers that can control their runtime context may honor it.
func WithRequestedContextLength(ctx context.Context, contextLength int) context.Context {
	if contextLength <= 0 {
		return ctx
	}
	return context.WithValue(ctx, requestedContextLengthKey{}, contextLength)
}

// RequestedContextLengthFromContext returns the positive context length attached
// by WithRequestedContextLength, or 0 when none was requested.
func RequestedContextLengthFromContext(ctx context.Context) int {
	v, _ := ctx.Value(requestedContextLengthKey{}).(int)
	if v <= 0 {
		return 0
	}
	return v
}
