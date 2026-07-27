package modelrepo

import (
	"errors"
	"fmt"
	"strings"
)

// Typed provider-error sentinels. Providers translate their documented error
// shapes into these via ClassifyProviderError (or provider-local mapping for
// SDK-typed errors), so callers can errors.Is on the failure class instead of
// grepping message strings. Context-recovery work depends on
// ErrContextLengthExceeded being distinguishable.
var (
	// ErrContextLengthExceeded means the request (prompt + requested output)
	// does not fit the model's context window. Retrying unchanged cannot
	// succeed; the caller must shrink the request or pick a larger model.
	ErrContextLengthExceeded = errors.New("request exceeds the model's context window")
	// ErrRateLimited means the provider refused the request due to rate or
	// capacity limits (HTTP 429, Anthropic 529 overloaded, Bedrock throttling,
	// Gemini RESOURCE_EXHAUSTED). Retrying after a backoff can succeed.
	ErrRateLimited = errors.New("provider rate limit or capacity limit hit")
)

// contextLimitMarkers are the documented provider phrasings/codes for a
// context-window overflow, lowercased. Sources: OpenAI error code
// context_length_exceeded and "maximum context length" message; Anthropic
// invalid_request_error "prompt is too long" / "input length and max_tokens
// exceed context limit"; Gemini INVALID_ARGUMENT "token count" / "exceeds the
// maximum number of tokens"; Bedrock ValidationException "input is too long";
// ollama/vllm token-limit strings ("context length", "kv cache").
var contextLimitMarkers = []string{
	"context_length_exceeded",
	"maximum context length",
	"prompt is too long",
	"exceed context limit",
	"exceeds context limit",
	"input is too long",
	"input length and `max_tokens` exceed",
	"input length and max_tokens exceed",
	"token count exceeds",
	"exceeds the maximum number of tokens",
	"please reduce the length",
	"context window",
	"context length",
	"kv cache",
}

// IsContextLimitMessage reports whether a provider error code or message
// matches a documented context-window-overflow phrasing.
func IsContextLimitMessage(s string) bool {
	s = strings.ToLower(s)
	if s == "" {
		return false
	}
	for _, m := range contextLimitMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// rateLimitMarkers are documented rate/capacity error codes that can arrive
// without a matching HTTP status: Gemini's RESOURCE_EXHAUSTED gRPC status,
// Anthropic's rate_limit_error/overloaded_error types, AWS throttling.
var rateLimitMarkers = []string{
	"resource_exhausted",
	"rate_limit_error",
	"overloaded_error",
	"throttl",
}

func isRateLimitCode(s string) bool {
	s = strings.ToLower(s)
	if s == "" {
		return false
	}
	for _, m := range rateLimitMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ClassifyProviderError wraps err with the matching typed sentinel based on
// what the provider reported: HTTP 429/529 → ErrRateLimited; an error code or
// message matching a documented context-overflow phrasing (only trusted on a
// 4xx/unknown status — a 5xx is a provider fault, not an overflow) →
// ErrContextLengthExceeded. Anything else returns err unchanged, so callers
// never lose the original detail.
func ClassifyProviderError(err error, httpStatus int, code, message string) error {
	if err == nil {
		return nil
	}
	if httpStatus == 429 || httpStatus == 529 || isRateLimitCode(code) {
		return fmt.Errorf("%w: %w", ErrRateLimited, err)
	}
	if httpStatus == 0 || (httpStatus >= 400 && httpStatus < 500) {
		if IsContextLimitMessage(code) || IsContextLimitMessage(message) {
			return fmt.Errorf("%w: %w", ErrContextLengthExceeded, err)
		}
	}
	return err
}
