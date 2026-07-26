package libacp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"testing"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
)

func TestUnit_IsStartupError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"start-failed sentinel", libacp.ErrAgentStartFailed, true},
		{"wrapped start-failed", fmt.Errorf("spawn: %w", libacp.ErrAgentStartFailed), true},
		{"exec not found", exec.ErrNotFound, true},
		{"wrapped exec not found", fmt.Errorf("start foo: %w", exec.ErrNotFound), true},
		{"idle timeout is not startup", libacp.ErrIdleTimeout, false},
		{"connection closed is not startup", libacp.ErrConnectionClosed, false},
		{"context canceled is not startup", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, libacp.IsStartupError(tc.err))
		})
	}
}

func TestUnit_IsTimeoutError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("turn: %w", context.DeadlineExceeded), true},
		{"idle timeout", libacp.ErrIdleTimeout, true},
		{"wrapped idle timeout", fmt.Errorf("turn: %w", libacp.ErrIdleTimeout), true},
		{"context canceled is not a timeout", context.Canceled, false},
		{"start failed is not a timeout", libacp.ErrAgentStartFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, libacp.IsTimeoutError(tc.err))
		})
	}
}

func TestUnit_IsRetryableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled never retryable", context.Canceled, false},
		{"wrapped canceled never retryable", fmt.Errorf("x: %w", context.Canceled), false},
		{"startup never retryable", libacp.ErrAgentStartFailed, false},
		{"exec not found never retryable", exec.ErrNotFound, false},
		{"deadline exceeded retryable", context.DeadlineExceeded, true},
		{"idle timeout retryable", libacp.ErrIdleTimeout, true},
		{"connection closed retryable", libacp.ErrConnectionClosed, true},
		{"no displayable output retryable", libacp.ErrNoDisplayableOutput, true},
		{"wrapped no output retryable", fmt.Errorf("%w (stopReason=end_turn)", libacp.ErrNoDisplayableOutput), true},
		{"EOF retryable", io.EOF, true},
		{"closed pipe retryable", io.ErrClosedPipe, true},
		{"EPIPE retryable", syscall.EPIPE, true},
		{"ECONNRESET retryable", syscall.ECONNRESET, true},
		{"broken pipe string retryable", errors.New("write |1: broken pipe"), true},
		{"connection reset string retryable", errors.New("read: connection reset by peer"), true},
		{"file already closed string retryable", errors.New("read foo: file already closed"), true},
		{"plain protocol error not retryable", errors.New("method not found"), false},
		{"rpc error not retryable", libacp.NewError(libacp.ErrInvalidParams, "bad"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, libacp.IsRetryableError(tc.err))
		})
	}
}
