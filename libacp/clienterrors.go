package libacp

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
)

// Client-side failure sentinels for a driver of a ClientSideConnection;
// classify with IsStartupError / IsTimeoutError / IsRetryableError rather
// than string-matching.
var (
	// ErrAgentStartFailed marks a failure to launch or initialize the agent
	// subprocess; not retryable (see IsStartupError).
	ErrAgentStartFailed = errors.New("libacp: agent start failed")

	// ErrIdleTimeout marks a turn that produced no session/update or result
	// past a driver's idle deadline, distinct from an overall context deadline.
	ErrIdleTimeout = errors.New("libacp: agent idle timeout")

	// ErrNoDisplayableOutput marks a turn that stopped with a normal reason
	// but never produced a renderable agent message; detect via TurnTracker.
	ErrNoDisplayableOutput = errors.New("libacp: prompt turn produced no displayable output")
)

// IsStartupError reports whether err means the agent could not be started or
// is unusable as configured — not fixable by retrying.
func IsStartupError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrAgentStartFailed) || errors.Is(err, exec.ErrNotFound)
}

// IsTimeoutError reports whether err is a context deadline or idle-watchdog
// timeout, matched by ErrRequestTimeout code since a serialized deadline
// loses its Go identity crossing the wire.
func IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrIdleTimeout) {
		return true
	}
	var rpcErr *Error
	return errors.As(err, &rpcErr) && rpcErr.Code == ErrRequestTimeout
}

// IsRetryableError reports whether retrying the turn might succeed: timeouts,
// a dropped transport, and an empty turn are retryable; cancellation and
// startup failures are not.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || IsStartupError(err) {
		return false
	}
	if IsTimeoutError(err) {
		return true
	}
	if errors.Is(err, ErrConnectionClosed) ||
		errors.Is(err, ErrNoDisplayableOutput) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "file already closed")
}
