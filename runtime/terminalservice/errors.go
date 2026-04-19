package terminalservice

import "errors"

var (
	// ErrDisabled is returned when the terminal feature is off.
	ErrDisabled = errors.New("terminal: disabled")
	// ErrNotImplemented is returned on platforms without PTY support (e.g. Windows).
	ErrNotImplemented = errors.New("terminal: not implemented on this platform")
	// ErrSessionNotFound is returned for unknown session IDs.
	ErrSessionNotFound = errors.New("terminal: session not found")
	// ErrTooManySessions is returned when the per-process session slot is occupied.
	ErrTooManySessions = errors.New("terminal: too many concurrent sessions")
)
