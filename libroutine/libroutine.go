// Package routine provides utilities for managing recurring tasks (routines)
// with circuit breaker protection.
//
// The primary purpose of this package is to provide a robust and managed way to run
// recurring background tasks that might fail, especially those interacting with
// external services or resources. It combines two key concepts:
//
// 1. Circuit Breaker (`Routine`): This pattern prevents an application from
// repeatedly performing an operation that's likely to fail. When failures reach a
// threshold, the "circuit" opens, and calls are blocked for a set period (`resetTimeout`).
// After the timeout, it enters a "half-open" state, allowing one test call.
// If that succeeds, the circuit closes; otherwise, it re-opens. This provides:
//   - Fault Tolerance: Prevents cascading failures by isolating problems.
//   - Resource Protection: Stops wasting resources (CPU, network, memory, API quotas)
//     on calls that are continuously failing.
//   - Automatic Recovery: Gives the failing service/resource time to recover before
//     trying again automatically.
//
// 2. Managed Background Loops (`group`): This component manages multiple circuit breakers
// (`Routine` instances) identified by unique keys. It handles the lifecycle of running
// the associated task (`fn`) in a background goroutine, ensuring:
//   - Organization: Keeps track of different background tasks centrally.
//   - Deduplication: Ensures only one instance of a loop runs for a given key,
//     even if `StartLoop` is called multiple times for the same key.
//   - Control: Allows periodic execution (`interval`), on-demand triggering
//     (`ForceUpdate`), context-based cancellation, and manual state resets (`ResetRoutine`).
//
// In essence, use `routine` to reliably run background jobs that need to be
// resilient to temporary failures without overwhelming either your application or
// the dependencies they rely on. See `group.StartLoop` for the primary entry point
// for running managed routines.
//
// # Job chains (Runner)
//
// Runner, Job, and Schedule build condition-gated, chainable operations on
// top of a Routine: Runner wraps one Routine so a job chain gets the same
// circuit-breaker protection as any other routine, adds a single-flight
// guard so a slow run is never overlapped by its own next trigger, and
// exposes three ways to fire that guarded execution — Run/Trigger directly,
// StartSchedule on an arbitrary Schedule (interface-compatible with
// robfig/cron/v3's cron.Schedule, so real cron syntax is a parser away
// without libroutine depending on one), or SubscribeMessenger to react to a
// github.com/contenox/contenox/libbus.Messenger subject.
//
// Condition and Operation are plain funcs, so a Job can drive anything
// (including github.com/contenox/contenox/libprocess), and every trigger
// path runs through the same Routine — a chain that starts failing
// repeatedly opens its circuit and backs off instead of being retried every
// tick or event forever.
package libroutine
