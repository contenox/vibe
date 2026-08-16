package libroutine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/contenox/contenox/libtracker"
)

// ErrAlreadyRunning is returned by Run when the Runner's job chain is already
// executing.
var ErrAlreadyRunning = errors.New("libroutine: job is already running")

// RunnerOption configures a Runner at construction.
type RunnerOption func(*Runner)

// WithResultHook registers fn to be called with the RunResult of every run that
// actually executed the job. fn is called synchronously and must not block.
func WithResultHook(fn func(*RunResult)) RunnerOption {
	return func(r *Runner) { r.hook = fn }
}

// WithTracker wires an ActivityTracker to observe every Run. Without it, a
// Runner uses libtracker.NoopTracker.
func WithTracker(tracker libtracker.ActivityTracker) RunnerOption {
	return func(r *Runner) { r.tracker = tracker }
}

// Runner drives one Job's execution through a dedicated Routine, with a
// single-flight guard so a slow run is never overlapped by its own next
// trigger. It is safe for concurrent use.
type Runner struct {
	job     *Job
	routine *Routine

	mu      sync.Mutex
	running bool

	hook    func(*RunResult)
	tracker libtracker.ActivityTracker
}

// NewRunner returns a Runner for job, protected by a Routine constructed with
// threshold and resetTimeout. The job chain is not started.
func NewRunner(job *Job, threshold int, resetTimeout time.Duration, opts ...RunnerOption) *Runner {
	r := &Runner{
		job:     job,
		routine: NewRoutine(threshold, resetTimeout),
		tracker: libtracker.NoopTracker{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run executes the job chain synchronously through the Runner's Routine. It
// returns a nil result with ErrAlreadyRunning or ErrCircuitOpen when the chain
// is already executing or the circuit is open.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	var res *RunResult
	execErr := r.routine.Execute(ctx, func(ctx context.Context) error {
		res = r.job.run(ctx)
		return res.firstErr()
	})
	if res == nil {
		return nil, execErr
	}

	reportErr, reportChange, end := r.tracker.Start(ctx, "run", "job", "name", r.job.Name)
	defer end()
	if err := res.firstErr(); err != nil {
		reportErr(err)
	} else {
		reportChange(r.job.Name, res)
	}

	if r.hook != nil {
		r.hook(res)
	}
	return res, nil
}

// Trigger launches Run in a background goroutine, silently dropping the request
// if the chain is already running or the circuit is open.
func (r *Runner) Trigger(ctx context.Context) {
	go func() { _, _ = r.Run(ctx) }()
}

// Running reports whether the job chain is currently executing.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}
