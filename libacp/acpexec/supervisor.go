package acpexec

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/contenox/beam/libacp"
)

// Supervisor keeps an agent subprocess alive across transient crashes by
// respawning it with backoff and re-running a caller-supplied session. It is
// the opt-in restart policy Spawn deliberately leaves out (Spawn is pure
// transport). A startup error (IsStartupError) — a missing binary or an agent
// that cannot initialize — is never retried, since looping on it only hides
// the misconfiguration; Serve surfaces it to the caller instead.
type Supervisor struct {
	// Command builds a fresh *exec.Cmd for each (re)start; required, since an
	// exec.Cmd cannot be reused once started.
	Command func(ctx context.Context) *exec.Cmd

	// MaxRestarts caps how many times Serve respawns after the first attempt.
	// Zero means one attempt with no restarts.
	MaxRestarts int

	// Backoff returns the delay before restart attempt n (1-based); nil means
	// no delay. Serve honors ctx cancellation while waiting.
	Backoff func(attempt int) time.Duration

	// OnRestart, if set, is called just before each genuine restart with the
	// 1-based restart number and the triggering error. Not called for a
	// startup or non-retryable error, so its absence alongside a returned
	// error signals a fatal failure (check libacp.IsStartupError).
	OnRestart func(attempt int, cause error)

	// SpawnOptions are forwarded to Spawn on every attempt (e.g. WithStderr).
	SpawnOptions []Option
}

// Serve spawns the agent and runs session against each live Process until the
// session succeeds (returns nil), ctx is cancelled, or a failure that is not
// worth retrying occurs. session receives a zero-based attempt counter (0 on
// the first spawn) so a restart can try session/resume before falling back to
// session/new.
//
// A spawn/start failure is wrapped as libacp.ErrAgentStartFailed and returned
// immediately (never retried). A session error is retried, up to MaxRestarts,
// only when libacp.IsRetryableError says so and it is not a startup error;
// otherwise it is returned as-is.
func (s *Supervisor) Serve(ctx context.Context, session func(ctx context.Context, proc *Process, attempt int) error) error {
	if s.Command == nil {
		return fmt.Errorf("acpexec: Supervisor.Command is required")
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		proc, err := Spawn(ctx, s.Command(ctx), s.SpawnOptions...)
		if err != nil {
			// A start failure is a startup error by definition; surface it
			// rather than loop, since a retry cannot cure a bad binary.
			return fmt.Errorf("%w: %w", libacp.ErrAgentStartFailed, err)
		}

		runErr := session(ctx, proc, attempt)
		_ = proc.Close()

		if runErr == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if libacp.IsStartupError(runErr) || !libacp.IsRetryableError(runErr) {
			return runErr
		}
		if attempt >= s.MaxRestarts {
			return runErr
		}

		if s.OnRestart != nil {
			s.OnRestart(attempt+1, runErr)
		}
		if s.Backoff != nil {
			if d := s.Backoff(attempt + 1); d > 0 {
				timer := time.NewTimer(d)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
		}
	}
}
