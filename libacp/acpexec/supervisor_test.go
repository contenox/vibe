package acpexec_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libacp/acpexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trueCmd returns a cheap, always-present subprocess; the Supervisor tests care
// about the session callback's result, not the process's own behavior.
func trueCmd(context.Context) *exec.Cmd { return exec.Command("cat") }

func TestSupervisor_SucceedsFirstAttemptNoRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var attempts []int
	sup := acpexec.Supervisor{Command: trueCmd, MaxRestarts: 3}
	err := sup.Serve(ctx, func(_ context.Context, _ *acpexec.Process, attempt int) error {
		attempts = append(attempts, attempt)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0}, attempts, "a succeeding session must run exactly once")
}

func TestSupervisor_RetriesRetryableErrorThenSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var restarts []int
	sup := acpexec.Supervisor{
		Command:     trueCmd,
		MaxRestarts: 3,
		OnRestart:   func(attempt int, _ error) { restarts = append(restarts, attempt) },
	}

	var attempts []int
	err := sup.Serve(ctx, func(_ context.Context, _ *acpexec.Process, attempt int) error {
		attempts = append(attempts, attempt)
		if attempt < 2 {
			return libacp.ErrConnectionClosed // retryable
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, attempts, "attempt counter must advance across restarts (resume-memory support)")
	assert.Equal(t, []int{1, 2}, restarts, "OnRestart fires once per restart with 1-based numbers")
}

func TestSupervisor_StartupErrorFromSessionNotRetried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var restartCalled bool
	sup := acpexec.Supervisor{
		Command:     trueCmd,
		MaxRestarts: 5,
		OnRestart:   func(int, error) { restartCalled = true },
	}

	calls := 0
	err := sup.Serve(ctx, func(context.Context, *acpexec.Process, int) error {
		calls++
		return libacp.ErrAgentStartFailed
	})
	require.ErrorIs(t, err, libacp.ErrAgentStartFailed)
	assert.Equal(t, 1, calls, "a startup error must not be retried")
	assert.False(t, restartCalled, "OnRestart must not fire for a fatal startup error")
}

func TestSupervisor_NonRetryableErrorNotRetried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sentinel := errors.New("method not found") // not retryable
	sup := acpexec.Supervisor{Command: trueCmd, MaxRestarts: 5}

	calls := 0
	err := sup.Serve(ctx, func(context.Context, *acpexec.Process, int) error {
		calls++
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, calls)
}

func TestSupervisor_ExhaustsMaxRestartsAndReturnsLastError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sup := acpexec.Supervisor{Command: trueCmd, MaxRestarts: 2}

	calls := 0
	last := errors.New("io: read/write on closed pipe: broken pipe")
	err := sup.Serve(ctx, func(context.Context, *acpexec.Process, int) error {
		calls++
		return last
	})
	require.ErrorIs(t, err, last)
	assert.Equal(t, 3, calls, "one initial attempt + MaxRestarts=2 restarts")
}

func TestSupervisor_AppliesBackoffAndHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var backoffAttempts []int
	sup := acpexec.Supervisor{
		Command:     trueCmd,
		MaxRestarts: 5,
		Backoff: func(attempt int) time.Duration {
			backoffAttempts = append(backoffAttempts, attempt)
			return 50 * time.Millisecond
		},
	}

	// Cancel while the first backoff is pending; Serve must return ctx.Err.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := sup.Serve(ctx, func(context.Context, *acpexec.Process, int) error {
		return libacp.ErrConnectionClosed
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second, "cancellation must interrupt the backoff wait")
	assert.Equal(t, []int{1}, backoffAttempts, "backoff is consulted with the 1-based restart number")
}

func TestSupervisor_SpawnFailureIsStartupError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sup := acpexec.Supervisor{
		Command:     func(context.Context) *exec.Cmd { return exec.Command("contenox-no-such-agent-binary-xyz") },
		MaxRestarts: 5,
	}

	sessionCalled := false
	err := sup.Serve(ctx, func(context.Context, *acpexec.Process, int) error {
		sessionCalled = true
		return nil
	})
	require.Error(t, err)
	assert.True(t, libacp.IsStartupError(err), "a failed spawn must classify as a startup error")
	assert.ErrorIs(t, err, libacp.ErrAgentStartFailed)
	assert.False(t, sessionCalled, "session must not run when the process could not start")
}

func TestSupervisor_MissingCommandErrors(t *testing.T) {
	sup := acpexec.Supervisor{}
	err := sup.Serve(context.Background(), func(context.Context, *acpexec.Process, int) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Command is required")
}

func TestSupervisor_CancelledContextBeforeStartReturnsCtxErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sup := acpexec.Supervisor{Command: trueCmd, MaxRestarts: 3}
	called := false
	err := sup.Serve(ctx, func(context.Context, *acpexec.Process, int) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
}
