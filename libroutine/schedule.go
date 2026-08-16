package libroutine

import (
	"context"
	"time"
)

// Schedule computes the next run time after t. It is interface-compatible with
// robfig/cron/v3's cron.Schedule.
type Schedule interface {
	Next(t time.Time) time.Time
}

type everySchedule struct{ d time.Duration }

func (e everySchedule) Next(t time.Time) time.Time { return t.Add(e.d) }

// Every returns a Schedule that fires at a fixed interval.
func Every(d time.Duration) Schedule { return everySchedule{d: d} }

// StartSchedule runs r.Trigger each time sched fires, until ctx is cancelled. It
// returns immediately, and a tick that lands mid-run is dropped rather than
// queued.
func (r *Runner) StartSchedule(ctx context.Context, sched Schedule) {
	go r.runSchedule(ctx, sched)
}

func (r *Runner) runSchedule(ctx context.Context, sched Schedule) {
	next := sched.Next(time.Now())
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-timer.C:
			r.Trigger(ctx)
			next = sched.Next(t)
			d := time.Until(next)
			if d < 0 {
				d = 0
			}
			timer.Reset(d)
		}
	}
}
