package modelrepo

import (
	"errors"
	"fmt"
	"strings"
)

// Typed provider-error sentinels. Providers translate their documented error
// shapes into these via ClassifyProviderError, so callers can errors.Is on
// the failure class instead of grepping message strings.
var (
	// ErrContextLengthExceeded means the request does not fit the model's
	// context window. Retrying unchanged cannot succeed.
	ErrContextLengthExceeded = errors.New("request exceeds the model's context window")
	// ErrRateLimited means the provider refused the request for rate or
	// capacity reasons. Retrying after a backoff can succeed.
	ErrRateLimited = errors.New("provider rate limit or capacity limit hit")
)

// contextLimitMarkers are documented provider phrasings/codes for a
// context-window overflow, lowercased (OpenAI context_length_exceeded,
// Anthropic "prompt is too long", Gemini "exceeds the maximum number of
// tokens", Bedrock "input is too long", ollama/vllm "context length"/"kv cache").
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

// rateLimitMarkers are rate/capacity error codes that can arrive without a
// matching HTTP status (Gemini RESOURCE_EXHAUSTED, Anthropic
// rate_limit_error/overloaded_error, AWS throttling).
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

// ClassifyProviderError wraps err with the matching typed sentinel: HTTP
// 429/529 -> ErrRateLimited; a context-overflow phrasing on a 4xx/unknown
// status (a 5xx is a provider fault, not an overflow) -> ErrContextLengthExceeded.
// Anything else returns err unchanged.
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
