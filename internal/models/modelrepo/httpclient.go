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

// DefaultCallTimeout bounds one non-streaming provider call end-to-end
// (connect, request, and full response body).
//
// It covers the whole DoWithRetry loop, not one attempt: every retry and every
// honoured Retry-After is charged against the same budget. At 120s a single
// rate-limited response carrying "Retry-After: 60" left barely a third of the
// budget for the two remaining attempts, so the cap fired on a provider that
// was merely busy. Sized here for a slow reasoning model returning a large
// body after a rate-limit wait; a call that genuinely hangs is caught by the
// turn deadline above it.
const DefaultCallTimeout = 10 * time.Minute

// NonStreamingContext derives the context for a non-streaming provider call:
// the caller's ctx capped at DefaultCallTimeout. Callers must defer cancel.
func NonStreamingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, DefaultCallTimeout)
}

// httpRetryMaxAttempts caps retries at two after the initial attempt.
const httpRetryMaxAttempts = 3

// retryableStatus covers rate limits (429), Anthropic overload (529), and
// transient server faults (5xx); never used mid-stream.
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
// 429/529/5xx responses with backoff honoring Retry-After. build is called
// once per attempt so the request body is fresh each time; the final
// response is returned unconsumed, and transport errors return as-is
// without retry. Streaming calls must never go through this helper.
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
