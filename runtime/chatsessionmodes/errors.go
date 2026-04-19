package chatsessionmodes

import "errors"

var (
	errEmptyThread = errors.New("internal error: empty thread before injection")
	errLastNotUser = errors.New("internal error: expected last message to be user role")

	// ErrMissingModelProvider is returned when default-model / default-provider are unset and query overrides are empty.
	ErrMissingModelProvider = errors.New("missing model or provider for task chain")
)
