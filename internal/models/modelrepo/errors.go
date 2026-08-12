package modelrepo

import (
	"errors"
	"fmt"
	"strings"
)

// Typed provider-error sentinels; providers translate documented error shapes into these via ClassifyProviderError.
var (
	// ErrContextLengthExceeded means the request does not fit the model's context window.
	ErrContextLengthExceeded = errors.New("request exceeds the model's context window")
	// ErrRateLimited means the provider refused the request for rate or capacity reasons.
	ErrRateLimited = errors.New("provider rate limit or capacity limit hit")
	// ErrModelNotFoundOnBackend means the backend does not serve the requested model; terminal for that backend, not for the request.
	ErrModelNotFoundOnBackend = errors.New("backend does not serve the requested model")
	// ErrModelAccessDenied means the backend refused access to the requested model; terminal for that backend, not for the request.
	ErrModelAccessDenied = errors.New("backend denied access to the requested model")
)

// IsBackendTerminal reports whether err is terminal for the backend that produced it, not necessarily for the request.
func IsBackendTerminal(err error) bool {
	return errors.Is(err, ErrModelNotFoundOnBackend) || errors.Is(err, ErrModelAccessDenied)
}

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

var notFoundCodeMarkers = []string{"not_found"}

var notFoundMessageMarkers = []string{"not found", "does not exist"}

var accessDeniedCodeMarkers = []string{"permission_denied", "access_denied"}

func containsMarker(s string, markers []string) bool {
	s = strings.ToLower(s)
	if s == "" {
		return false
	}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ClassifyProviderError wraps err with the matching typed sentinel based on HTTP status and provider code/message, or returns err unchanged.
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
	if httpStatus == 404 || containsMarker(code, notFoundCodeMarkers) ||
		(httpStatus == 0 && containsMarker(message, notFoundMessageMarkers)) {
		return fmt.Errorf("%w: %w", ErrModelNotFoundOnBackend, err)
	}
	if httpStatus == 403 || containsMarker(code, accessDeniedCodeMarkers) {
		return fmt.Errorf("%w: %w", ErrModelAccessDenied, err)
	}
	return err
}
