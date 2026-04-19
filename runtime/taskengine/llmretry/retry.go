// Package llmretry wraps a single LLM call with classified retry, exponential
// backoff, and an optional model fallback. It has no contenox-internal
// dependencies and is safe to use from any task handler.
//
// The classifier inspects formatted error strings because contenox's provider
// clients (internal/modelrepo/{openai,vllm,gemini,...}) return errors as
// fmt.Errorf-wrapped strings of the shape:
//
//	"OpenAI API returned non-200 status: 429, body: …"
//
// Substring matching keeps llmretry decoupled from any specific provider.
package llmretry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ErrorClass is a coarse classification of an LLM call failure used for retry
// and fallback decisions. Empty (ClassNone) means no error.
type ErrorClass string

const (
	// ClassNone is returned for nil errors.
	ClassNone ErrorClass = ""
	// ClassRateLimit is HTTP 429 / 529 (Anthropic overload). Retried with a
	// longer floor (RateLimitMinWait).
	ClassRateLimit ErrorClass = "rate_limit"
	// ClassServerError is HTTP 5xx. Retried with normal backoff.
	ClassServerError ErrorClass = "server_error"
	// ClassTimeout is context.DeadlineExceeded or i/o timeout. Retried.
	ClassTimeout ErrorClass = "timeout"
	// ClassAuth is HTTP 401/403 or "invalid api key". Never retried.
	ClassAuth ErrorClass = "auth"
	// ClassCapacity is a context-length / token-overflow error. Never retried;
	// the caller (e.g. planservice) may treat this as a signal to replan.
	ClassCapacity ErrorClass = "capacity"
	// ClassCanceled is context.Canceled. Never retried.
	ClassCanceled ErrorClass = "canceled"
	// ClassPermanent is anything that does not match a known transient pattern.
	// Never retried by default.
	ClassPermanent ErrorClass = "permanent"
)

// IsRetryable reports whether an error of class c warrants another attempt.
func (c ErrorClass) IsRetryable() bool {
	switch c {
	case ClassRateLimit, ClassServerError, ClassTimeout:
		return true
	default:
		return false
	}
}

// ClassifyError inspects err for known transient classes. Returns ClassNone
// for nil errors. Detection is intentionally permissive (substring match
// against the formatted error) because providers do not expose typed errors.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ClassNone
	}
	if errors.Is(err, context.Canceled) {
		return ClassCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}
	s := strings.ToLower(err.Error())
	switch {
	case containsAny(s, "429", "too many requests", "rate limit", "rate-limit", "529", "overloaded"):
		return ClassRateLimit
	case containsAny(s, "401", "403", "unauthorized", "forbidden", "invalid api key", "authentication"):
		return ClassAuth
	case containsAny(s, "context length", "context window", "maximum context", "exceeds context", "tokens exceed", "token count "):
		return ClassCapacity
	case containsAny(s, "500", "502", "503", "504", "internal server error", "bad gateway", "service unavailable", "gateway timeout"):
		return ClassServerError
	case containsAny(s, "i/o timeout", "deadline exceeded", "timed out"):
		return ClassTimeout
	}
	return ClassPermanent
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Duration is a time.Duration that JSON-decodes from either a numeric
// nanosecond value or a duration string ("1s", "500ms", "2m"). This lets
// chain JSON files express timeouts in human form.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON serializes as a duration string for readability.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a JSON number (interpreted as nanoseconds, the
// stdlib default) or a JSON string parsed with time.ParseDuration.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string or number: %w", err)
	}
	*d = Duration(n)
	return nil
}

// RetryPolicy controls Do's retry/backoff/fallback behavior. The zero value
// disables retry (MaxAttempts = 0 → 1 attempt total).
type RetryPolicy struct {
	// MaxAttempts is the total attempts including the first. 0 or 1 disables retry.
	MaxAttempts int `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	// InitialBackoff is the wait before the second attempt; doubled (capped at
	// MaxBackoff) before each subsequent attempt. Defaults to 500ms when zero.
	InitialBackoff Duration `yaml:"initial_backoff,omitempty" json:"initial_backoff,omitempty"`
	// MaxBackoff caps the exponential backoff. 0 = no cap.
	MaxBackoff Duration `yaml:"max_backoff,omitempty" json:"max_backoff,omitempty"`
	// Jitter is a 0..1 fraction added to backoff (uniform random).
	Jitter float64 `yaml:"jitter,omitempty" json:"jitter,omitempty"`
	// RateLimitMinWait sets a floor for ClassRateLimit backoff.
	RateLimitMinWait Duration `yaml:"rate_limit_min_wait,omitempty" json:"rate_limit_min_wait,omitempty"`
	// FallbackModelID is the alternate model id used after FallbackAfter
	// consecutive failures. Empty disables fallback.
	FallbackModelID string `yaml:"fallback_model_id,omitempty" json:"fallback_model_id,omitempty"`
	// FallbackAfter is the consecutive-failure threshold that triggers the
	// fallback swap. 0 disables fallback regardless of FallbackModelID.
	FallbackAfter int `yaml:"fallback_after,omitempty" json:"fallback_after,omitempty"`
}

// Outcome reports what happened during Do. It is set even on error so callers
// can record retry/fallback usage in caveats or for later replan decisions.
type Outcome struct {
	Attempts       int
	UsedFallback   bool
	LastErrorClass ErrorClass
	Elapsed        time.Duration
}

// Do invokes call with primaryModel, retrying on transient errors per p.
// After p.FallbackAfter consecutive failures, it switches to p.FallbackModelID
// (when set) for remaining attempts. Auth, capacity, canceled, and permanent
// errors never retry.
//
// call receives the model id to use; on fallback, that id is p.FallbackModelID.
// The caller's closure is responsible for plumbing the id into the underlying
// provider call (e.g. by overriding the Request.ModelNames slice).
func Do(ctx context.Context, p RetryPolicy, primaryModel string, call func(modelID string) (any, error)) (any, Outcome, error) {
	start := time.Now()
	out := Outcome{}
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	model := primaryModel
	consecutive := 0
	var lastErr error
	for i := 1; i <= attempts; i++ {
		if err := ctx.Err(); err != nil {
			out.LastErrorClass = ClassifyError(err)
			out.Elapsed = time.Since(start)
			return nil, out, err
		}
		if !out.UsedFallback && p.FallbackModelID != "" && p.FallbackAfter > 0 && consecutive >= p.FallbackAfter {
			model = p.FallbackModelID
			out.UsedFallback = true
			consecutive = 0
		}
		result, err := call(model)
		out.Attempts = i
		if err == nil {
			out.LastErrorClass = ClassNone
			out.Elapsed = time.Since(start)
			return result, out, nil
		}
		class := ClassifyError(err)
		out.LastErrorClass = class
		lastErr = err
		if !class.IsRetryable() {
			out.Elapsed = time.Since(start)
			return nil, out, err
		}
		if i == attempts {
			break
		}
		consecutive++
		wait := backoffFor(p, i, class)
		if wait > 0 {
			select {
			case <-ctx.Done():
				out.LastErrorClass = ClassifyError(ctx.Err())
				out.Elapsed = time.Since(start)
				return nil, out, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	out.Elapsed = time.Since(start)
	if lastErr != nil {
		return nil, out, lastErr
	}
	return nil, out, fmt.Errorf("llmretry: exhausted attempts with no error captured")
}

func backoffFor(p RetryPolicy, attempt int, class ErrorClass) time.Duration {
	base := p.InitialBackoff.D()
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	maxBackoff := p.MaxBackoff.D()
	wait := base
	for k := 1; k < attempt; k++ {
		wait *= 2
		if maxBackoff > 0 && wait > maxBackoff {
			wait = maxBackoff
			break
		}
	}
	if class == ClassRateLimit && p.RateLimitMinWait.D() > wait {
		wait = p.RateLimitMinWait.D()
	}
	if p.Jitter > 0 {
		wait += time.Duration(rand.Float64() * p.Jitter * float64(wait))
	}
	return wait
}
