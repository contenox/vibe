package libroutine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/contenox/contenox/libtracker"
)

// ErrAlreadyRunning is returned by Run when the Runner's job chain is
// already executing. It is distinct from ErrCircuitOpen: this guards
// against overlapping a single slow run with itself, independent of the
// underlying Routine's failure-count state.
var ErrAlreadyRunning = errors.New("libroutine: job is already running")

// RunnerOption configures a Runner at construction.
type RunnerOption func(*Runner)

// WithResultHook registers fn to be called with the RunResult of every
// completed run (from Run, Trigger, a Schedule tick, or a
// SubscribeMessenger delivery) that actually executed the job — i.e. not
// when Execute short-circuited with ErrCircuitOpen or ErrAlreadyRunning. fn
// is called synchronously after the run completes and must not block.
//
// This is a lighter-weight alternative to WithTracker for callers that just
// want the typed RunResult tree; use WithTracker instead to integrate with
// this codebase's standard instrumentation seam (metrics, logging, tracing,
// audit trails).
func WithResultHook(fn func(*RunResult)) RunnerOption {
	return func(r *Runner) { r.hook = fn }
}

// WithTracker wires an ActivityTracker to observe every Run: Start is
// called when a run begins (after the single-flight and circuit-breaker
// gates pass), reportErr is called if the job chain failed or the circuit
// was open, reportChange is called with the job's Name and its RunResult on
// success, and end always fires. Without WithTracker, a Runner uses
// libtracker.NoopTracker.
func WithTracker(tracker libtracker.ActivityTracker) RunnerOption {
	return func(r *Runner) { r.tracker = tracker }
}

// Runner drives one Job's execution through a dedicated Routine, so a job
// chain gets the same circuit-breaker protection (see Routine) as any other
// managed operation in this package, plus a single-flight guard so a slow
// run is never overlapped by its own next trigger. Runner is safe for
// concurrent use.
type Runner struct {
	job     *Job
	routine *Routine

	mu      sync.Mutex
	running bool

	hook    func(*RunResult)
	tracker libtracker.ActivityTracker
}

// NewRunner returns a Runner for job, protected by a Routine constructed
// with threshold and resetTimeout (see NewRoutine). The job chain is not
// started; use Run, Trigger, StartSchedule, or SubscribeMessenger to drive
// it.
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

// Run executes the job chain synchronously through the Runner's Routine and
// returns its result. It returns ErrAlreadyRunning without running anything
// if the chain is already executing, and returns ErrCircuitOpen (see
// Routine.Execute) without running anything if the circuit breaker is open
// — in both cases the result is nil. Callers that want either condition
// silently absorbed instead of reported should use Trigger.
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
		// The circuit was open; Execute never called the job.
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

// Trigger requests a run without blocking the caller: it launches Run in a
// background goroutine and silently drops the request if the chain is
// already running or the circuit is open, rather than queuing or erroring
// on it. This is what StartSchedule ticks and SubscribeMessenger deliveries
// use.
func (r *Runner) Trigger(ctx context.Context) {
	go func() { _, _ = r.Run(ctx) }()
}

// Running reports whether the job chain is currently executing.
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}
