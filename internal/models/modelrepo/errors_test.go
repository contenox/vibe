package modelrepo

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnit_ClassifyProviderError(t *testing.T) {
	base := errors.New("boom")

	if err := ClassifyProviderError(base, 429, "", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 must map to ErrRateLimited, got %v", err)
	}
	if err := ClassifyProviderError(base, 529, "", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("529 must map to ErrRateLimited, got %v", err)
	}
	if err := ClassifyProviderError(base, 400, "", "RESOURCE_EXHAUSTED"); errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate-limit codes are matched on code, not message")
	}
	if err := ClassifyProviderError(base, 400, "RESOURCE_EXHAUSTED", ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("RESOURCE_EXHAUSTED code must map to ErrRateLimited, got %v", err)
	}

	if err := ClassifyProviderError(base, 400, "context_length_exceeded", ""); !errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("OpenAI code must map to ErrContextLengthExceeded, got %v", err)
	}
	if err := ClassifyProviderError(base, 400, "", "prompt is too long: 250000 tokens > 200000 maximum"); !errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("Anthropic phrasing must map to ErrContextLengthExceeded, got %v", err)
	}
	if err := ClassifyProviderError(base, 400, "INVALID_ARGUMENT", "The input token count exceeds the maximum number of tokens allowed"); !errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("Gemini phrasing must map to ErrContextLengthExceeded, got %v", err)
	}
	if err := ClassifyProviderError(base, 500, "", "maximum context length"); errors.Is(err, ErrContextLengthExceeded) {
		t.Fatalf("a 5xx is a provider fault, never a context overflow")
	}
	if err := ClassifyProviderError(base, 400, "", "invalid api key"); errors.Is(err, ErrContextLengthExceeded) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("unrelated errors must pass through unclassified")
	}
	if got := ClassifyProviderError(nil, 429, "", ""); got != nil {
		t.Fatalf("nil error stays nil")
	}
}

// TestUnit_DoWithRetry_HonorsRetryAfter: a 429 with Retry-After-Ms is retried
// (fresh request body each attempt) and the retry succeeds.
func TestUnit_DoWithRetry_HonorsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(t.Context(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 after retry, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("want exactly one retry, got %d calls", calls.Load())
	}
}

// TestUnit_DoWithRetry_GivesUp: retries are bounded and the final response is
// returned unconsumed so the caller can classify it.
func TestUnit_DoWithRetry_GivesUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	resp, err := DoWithRetry(t.Context(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("final response must be surfaced, got %d", resp.StatusCode)
	}
	if calls.Load() != int32(httpRetryMaxAttempts) {
		t.Fatalf("want %d bounded attempts, got %d", httpRetryMaxAttempts, calls.Load())
	}
}

func TestUnit_RetryAfterDelay(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	if got := RetryAfterDelay(h, time.Second); got != 3*time.Second {
		t.Fatalf("Retry-After seconds: got %v", got)
	}
	h = http.Header{}
	h.Set("Retry-After-Ms", "250")
	if got := RetryAfterDelay(h, time.Second); got != 250*time.Millisecond {
		t.Fatalf("Retry-After-Ms: got %v", got)
	}
	if got := RetryAfterDelay(http.Header{}, 7*time.Second); got != 7*time.Second {
		t.Fatalf("fallback: got %v", got)
	}
}

// TestUnit_ProviderSentinels_AreDistinct guards against accidentally wrapping
// one sentinel in the other.
func TestUnit_ProviderSentinels_AreDistinct(t *testing.T) {
	wrapped := fmt.Errorf("%w: detail", ErrContextLengthExceeded)
	if errors.Is(wrapped, ErrRateLimited) {
		t.Fatal("sentinels must be independent")
	}
}
