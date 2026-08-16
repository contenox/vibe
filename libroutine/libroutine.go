// Package libroutine runs recurring background tasks under circuit-breaker
// protection: Routine is the breaker, group manages one keyed loop per task, and
// Runner, Job and Schedule build condition-gated job chains on top of a Routine
// that can be fired directly, on a Schedule, or from a libbus subject.
package libroutine
