package libroutine

import (
	"context"
	"fmt"
	"time"
)

// Condition gates whether a Job's Operation runs. A nil Condition on a Job
// always proceeds.
type Condition func(ctx context.Context) (bool, error)

// Operation is the work a Job performs once its Condition allows it.
type Operation func(ctx context.Context) error

// Job is one step in a chain: check Condition, run Operation, and if both
// succeed continue into Next. A Job is driven by a Runner.
type Job struct {
	// Name identifies this job in RunResult and error messages.
	Name string
	// Condition, if set, is evaluated before Operation. A false result skips
	// Operation and Next without failing the run.
	Condition Condition
	Operation Operation
	// Next, if set, runs after Operation succeeds.
	Next *Job
}

// RunResult reports the outcome of running a Job, including its chain.
type RunResult struct {
	Name     string
	Skipped  bool
	Err      error
	Duration time.Duration
	// Next is the chained job's result, set only when Next was run.
	Next *RunResult
}

// Failed reports whether this result or any later in its chain errored.
func (r *RunResult) Failed() bool {
	return r.firstErr() != nil
}

func (r *RunResult) firstErr() error {
	for n := r; n != nil; n = n.Next {
		if n.Err != nil {
			return n.Err
		}
	}
	return nil
}

func (j *Job) run(ctx context.Context) *RunResult {
	start := time.Now()
	res := &RunResult{Name: j.Name}
	defer func() { res.Duration = time.Since(start) }()

	if j.Condition != nil {
		ok, err := j.Condition(ctx)
		if err != nil {
			res.Err = fmt.Errorf("libroutine: condition for %q: %w", j.Name, err)
			return res
		}
		if !ok {
			res.Skipped = true
			return res
		}
	}

	if j.Operation != nil {
		if err := j.Operation(ctx); err != nil {
			res.Err = fmt.Errorf("libroutine: operation for %q: %w", j.Name, err)
			return res
		}
	}

	if j.Next != nil {
		select {
		case <-ctx.Done():
			return res
		default:
			res.Next = j.Next.run(ctx)
		}
	}
	return res
}
