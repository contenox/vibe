package libtracker

import (
	"context"
	"fmt"
	"math/rand/v2"
)

var ContextKeyRequestID = contextKey("request_id")
var ContextKeyTraceID = contextKey("trace_id")
var ContextKeySpanID = contextKey("span_id")

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
// Call this at the top of any CLI command or goroutine entry-point that
// doesn't already have a request ID so the tracker never logs SERVERBUG.
func WithNewRequestID(ctx context.Context) context.Context {
	id := fmt.Sprintf("cli-%016x", rand.Uint64())
	return context.WithValue(ctx, ContextKeyRequestID, id)
}
