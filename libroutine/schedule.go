package libroutine

import (
	"context"
	"time"
)

// Schedule computes the next run time after t. It is interface-compatible
// with robfig/cron/v3's cron.Schedule, so a caller who wants real cron
// expression syntax can parse one with that package and pass the result
// here directly, without libroutine depending on a cron parser itself.
type Schedule interface {
	Next(t time.Time) time.Time
}

type everySchedule struct{ d time.Duration }

func (e everySchedule) Next(t time.Time) time.Time { return t.Add(e.d) }

// Every returns a Schedule that fires at a fixed interval, with no
// dependency beyond the standard library. Use it directly for simple
// polling-style jobs, or as a placeholder until a real cron expression is
// wired in via a Schedule implementation.
//
// Note this is a different tool than group.StartLoop's fixed interval: that
// method drives a single func(ctx) error directly under a Routine with no
// job-chain, condition, or cron-shaped scheduling around it. Reach for
// StartLoop when a plain recurring function is all you need, and for a
// Runner with Every (or a real Schedule) when you also want Job's condition
// gating or chaining.
func Every(d time.Duration) Schedule { return everySchedule{d: d} }

// StartSchedule runs r.Trigger each time sched fires, until ctx is
// cancelled. A tick that lands while the previous run is still in flight,
// or while the Runner's circuit breaker is open, is dropped (see Trigger)
// rather than queued. StartSchedule returns immediately; the schedule loop
// runs in a background goroutine that exits when ctx is done.
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
