package apiframework

import (
	"context"
	"net/http"
	"strings"

	"github.com/contenox/vibe/libtracker"
	"github.com/google/uuid"
)

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		ctx := context.WithValue(r.Context(), libtracker.ContextKeyRequestID, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TracingMiddleware extracts or generates trace and span IDs.
func TracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		traceID := ""
		spanID := ""

		// Check for W3C traceparent header
		traceparent := r.Header.Get("traceparent")
		if traceparent != "" {
			parts := strings.Split(traceparent, "-")
			if len(parts) == 4 {
				traceID = parts[1]
				spanID = parts[2]
			}
		}

		// If no trace ID was found in the header, generate a new one
		if traceID == "" {
			traceID = uuid.New().String()
			spanID = uuid.New().String()[:16]
		}

		ctx = context.WithValue(ctx, libtracker.ContextKeyTraceID, traceID)
		ctx = context.WithValue(ctx, libtracker.ContextKeySpanID, spanID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
