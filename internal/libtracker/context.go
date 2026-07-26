package libtracker

import (
	"context"
	"fmt"
	"math/rand/v2"
)

var ContextKeyRequestID = contextKey("request_id")
var ContextKeyTraceID = contextKey("trace_id")
var ContextKeySpanID = contextKey("span_id")

// CopyTrackingValues copies the tracking values from src to dst.
func CopyTrackingValues(src context.Context, dst context.Context) context.Context {
	requestID := src.Value(ContextKeyRequestID)
	traceID := src.Value(ContextKeyTraceID)
	spanID := src.Value(ContextKeySpanID)
	ctx := context.WithValue(dst, ContextKeyRequestID, requestID)
	ctx = context.WithValue(ctx, ContextKeyTraceID, traceID)
	ctx = context.WithValue(ctx, ContextKeySpanID, spanID)
	return ctx
}

// WithNewRequestID stamps a fresh random request ID into ctx.
// Call this at the top of any command or goroutine entry-point that
// doesn't already have a request ID so the tracker never logs SERVERBUG.
//
// math/rand is deliberate, not an oversight: request IDs are correlation keys
// only. Nothing in this repo authenticates or authorizes on one — the HTTP edge
// (apiframework.RequestIDMiddleware) will happily adopt an attacker-supplied
// X-Request-ID header verbatim, so unpredictability could never have been a
// property anything relied on. The remaining requirement is collision
// avoidance, which 64 bits of math/rand/v2 (per-process seeded from the
// runtime's random source) satisfies. Do NOT reuse these IDs as tokens,
// nonces, or idempotency keys; mint those with crypto/rand at the point of use.
func WithNewRequestID(ctx context.Context) context.Context {
	id := fmt.Sprintf("cli-%016x", rand.Uint64())
	return context.WithValue(ctx, ContextKeyRequestID, id)
}
