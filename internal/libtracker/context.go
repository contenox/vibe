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

// WithNewRequestID stamps a fresh random request ID into ctx. Call this at
// any entry point lacking one so the tracker never logs SERVERBUG.
//
// Uses math/rand/v2, not crypto/rand, deliberately: request IDs are
// correlation keys only, never authenticated or authorized on, so the only
// requirement is collision avoidance. Do not reuse these as tokens, nonces,
// or idempotency keys — mint those with crypto/rand.
func WithNewRequestID(ctx context.Context) context.Context {
	id := fmt.Sprintf("cli-%016x", rand.Uint64())
	return context.WithValue(ctx, ContextKeyRequestID, id)
}
