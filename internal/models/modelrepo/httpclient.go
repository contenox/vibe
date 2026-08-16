package modelrepo

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SharedHTTPClient is the process-wide HTTP client for provider and catalog
// calls. It carries no overall Timeout, since streaming responses run for
// minutes; non-streaming calls bound themselves with NonStreamingContext.
var SharedHTTPClient = &http.Client{}

// DefaultCallTimeout bounds one non-streaming provider call end-to-end. It
// covers the whole DoWithRetry loop, not one attempt: every retry and every
// honoured Retry-After is charged against the same budget.
const DefaultCallTimeout = 10 * time.Minute

// NonStreamingContext derives the context for a non-streaming provider call:
// the caller's ctx capped at DefaultCallTimeout. Callers must defer cancel.
func NonStreamingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, DefaultCallTimeout)
}

const httpRetryMaxAttempts = 3

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == 529 || code >= 500
}

// RetryAfterDelay reads Retry-After-Ms (milliseconds) or Retry-After (seconds)
// from response headers, falling back to fallback when neither is present.
func RetryAfterDelay(h http.Header, fallback time.Duration) time.Duration {
	if ms := h.Get("Retry-After-Ms"); ms != "" {
		if n, err := strconv.ParseInt(ms, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
}

// DoWithRetry issues a non-streaming request built by build, retrying
// 429/529/5xx responses with backoff honoring Retry-After. Streaming calls must
// never go through this helper.
func DoWithRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	if client == nil {
		client = SharedHTTPClient
	}
	for attempt := 1; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt >= httpRetryMaxAttempts || !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		wait := RetryAfterDelay(resp.Header, time.Duration(attempt)*time.Second)
		// Drain a little so the connection can be reused, then drop the body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}
