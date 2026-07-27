package modelrepo

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SharedHTTPClient is the process-wide HTTP client for provider and catalog
// calls. It deliberately carries NO overall Timeout: streaming responses run
// for many minutes and an http.Client.Timeout would cut them mid-stream.
// Cancellation is the request context's job; non-streaming calls bound
// themselves with NonStreamingContext.
var SharedHTTPClient = &http.Client{}

// DefaultCallTimeout bounds one non-streaming provider call end-to-end
// (connect, request, and full response body).
const DefaultCallTimeout = 120 * time.Second

// NonStreamingContext derives the context for a non-streaming provider call:
// the caller's ctx capped at DefaultCallTimeout. Callers must defer cancel.
func NonStreamingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, DefaultCallTimeout)
}

// httpRetryMaxAttempts keeps the retry helper a helper, not a resilience
// framework: at most two retries after the initial attempt.
const httpRetryMaxAttempts = 3

// retryableStatus: rate limits (429), Anthropic overload (529), and transient
// server faults (5xx). Never used mid-stream — retrying applies only to calls
// whose response has not been consumed.
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

// DoWithRetry issues a NON-streaming request built by build, retrying
// 429/529/5xx responses with backoff that honors Retry-After. build is called
// once per attempt so the request body is fresh each time. The final response
// (success or not) is returned unconsumed; transport errors are returned
// as-is without retry (the failure mode there is ambiguous re-execution).
// Streaming calls must never go through this helper.
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
