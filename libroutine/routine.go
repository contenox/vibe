package libroutine

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// State represents the operational state of the Routine (circuit breaker).
// Circuit Breaker States:
//
//	 (Success)
//	 < Succeeded > --+
//	/               |
//
// [CLOSED] --- Failure Threshold Reached ---> [OPEN]
//
//	^                                          |
//	|                                          | Reset Timeout Elapsed
//	| Success (Test OK)                        V
//
// [HALF-OPEN] <---- Failure (Test Failed) ---[ Test Call ]
//
//	|
//	+-- Failure (Test Failed) --> (Reverts to OPEN)
type State int

const (
	// Closed allows operations to execute and counts failures.
	Closed State = iota
	// Open prevents operations from executing immediately. After a timeout,
	// it transitions to HalfOpen.
	Open
	// HalfOpen allows a single operation attempt. If successful, transitions to Closed.
	// If it fails, transitions back to Open.
	HalfOpen
)

// String returns a human-readable representation of the State.
func (s State) String() string {
	switch s {
	case Closed:
		return "Closed"
	case Open:
		return "Open"
	case HalfOpen:
		return "HalfOpen"
	default:
		return "Unknown"
	}
}

// ErrCircuitOpen is returned by Execute when the circuit breaker is in the Open state
// and blocking calls. Callers can use errors.Is(err, ErrCircuitOpen) to check.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Routine implements a circuit breaker pattern to protect functions from repeated failures.
// It tracks failures, opens the circuit when a threshold is reached, and attempts
// to reset automatically after a timeout (via the HalfOpen state).
// Routines are typically managed by the group but can be used standalone if needed.
type Routine struct {
	mu            sync.Mutex    // Protects access to the fields below
	state         State         // Current state (Closed, Open, HalfOpen)
	failureCount  int           // Consecutive failures count
	threshold     int           // Failure count needed to trip to Open state
	resetTimeout  time.Duration // Duration to stay in Open state before HalfOpen
	lastFailureAt time.Time     // Timestamp of the last recorded failure
	inTest        bool          // Flag used only in HalfOpen state to allow one test call
}

// NewRoutine creates a new circuit breaker (`Routine`) instance.
//
// Parameters:
//   - threshold: The number of consecutive failures required to move the state from Closed to Open. Must be greater than 0.
//   - resetTimeout: The period the circuit breaker will remain Open before transitioning to HalfOpen. Must be positive.
func NewRoutine(threshold int, resetTimeout time.Duration) *Routine {
	// log.Printf("Creating new routine with threshold: %d, reset timeout: %s", threshold, resetTimeout)
	return &Routine{
		threshold:    threshold,
		resetTimeout: resetTimeout,
		state:        Closed,
	}
}

// Allow checks if the circuit breaker permits an operation based on its current state.
// It's consulted by `Execute`. Direct use is uncommon but possible for manual checks.
//
// Returns:
//   - true: If the state is Closed, or if the state is HalfOpen and no test is ongoing.
//   - false: If the state is Open and the reset timeout hasn't elapsed, or if the state
//     is HalfOpen and a test operation is already in progress.
//
// Note: This method may transition the state from Open to HalfOpen if the timeout has passed.
func (rm *Routine) Allow() bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	switch rm.state {
	case Closed:
		return true
	case Open:
		if time.Since(rm.lastFailureAt) > rm.resetTimeout {
			//	log.Println("Reset timeout elapsed, transitioning to HalfOpen state")
			rm.state = HalfOpen
			rm.inTest = false
		} else {
			//	log.Println("Circuit breaker is open, request denied")
			return false
		}
	case HalfOpen:
		if rm.inTest {
			//	log.Println("HalfOpen state: Test request already in progress, request denied")
			return false
		}
	}

	if rm.state == HalfOpen && !rm.inTest {
		//	log.Println("HalfOpen state: Allowing test request")
		rm.inTest = true
	}
	return true
}

// MarkSuccess resets the circuit breaker after a successful call.
func (rm *Routine) MarkSuccess() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	switch rm.state {
	case Closed:
		rm.failureCount = 0
	case HalfOpen:
		//	log.Println("HalfOpen state: Test request succeeded, transitioning to Closed")
		rm.state = Closed
		rm.failureCount = 0
		rm.inTest = false
	}
}

// MarkFailure records a failed operation.
// If the state was Closed, it increments the failure count. If the count reaches
// the threshold, it transitions to Open.
// If the state was HalfOpen, it transitions back to Open.
// Called internally by `Execute` on failure.
func (rm *Routine) MarkFailure() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	switch rm.state {
	case Closed:
		rm.failureCount++
		// log.Printf("Failure recorded, count: %d", rm.failureCount)
		if rm.failureCount >= rm.threshold {
			// log.Println("Threshold reached, transitioning to Open state")
			rm.state = Open
			rm.lastFailureAt = time.Now().UTC()
		}
	case HalfOpen:
		// log.Println("HalfOpen state: Test request failed, reverting to Open state")
		rm.state = Open
		rm.lastFailureAt = time.Now().UTC()
		rm.inTest = false
	}
}

// Execute runs the provided function if allowed by the circuit breaker.
// Use this method when you need to protect a single, on-demand operation
// with circuit breaking, without requiring automatic looping or retries.
// For automatic retries, see `ExecuteWithRetry`. For recurring background tasks,
// see `group.StartLoop` or `Routine.Loop`.
func (rm *Routine) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if !rm.Allow() {
		// log.Println("Execution denied: Circuit breaker is open")
		return ErrCircuitOpen
	}

	err := fn(ctx)
	if err != nil {
		// log.Println("Execution failed, marking failure")
		rm.MarkFailure()
	} else {
		// log.Println("Execution succeeded, marking success")
		rm.MarkSuccess()
	}
	return err
}

// ExecuteWithRetry attempts to run the function `fn` using `Execute`, retrying
// on failure up to `iterations` times with a fixed `interval` between attempts.
// Retries stop early if `fn` succeeds or if the context `ctx` is cancelled.
// The circuit breaker logic applies to *each* attempt via `Execute`.
//
// Parameters:
//   - ctx: Context for cancellation. Retries stop if ctx is cancelled.
//   - interval: Fixed duration to wait between retry attempts. Consider jitter/backoff for production.
//   - iterations: Maximum number of execution attempts (including the first).
//   - fn: The function to execute.
//
// Returns:
//   - nil: If `fn` executes successfully within the allowed attempts.
//   - context.Canceled or context.DeadlineExceeded: If the context is cancelled/times out.
//   - error: The last error encountered (either from `fn` or `ErrCircuitOpen`) if all attempts fail.
func (rm *Routine) ExecuteWithRetry(ctx context.Context, interval time.Duration, iterations int, fn func(ctx context.Context) error) error {
	var err error
	for range iterations {
		if ctx.Err() != nil {
			// log.Println("Context cancelled, aborting retries")
			return context.Cause(ctx)
		}
		// log.Printf("Retry attempt %d", i+1)
		if err = rm.Execute(ctx, fn); err == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return err
}

// Loop continuously executes the function `fn` based on the circuit breaker state
// and a timer or trigger channel. This is the core execution logic used by `group.StartLoop`.
//
// The loop runs `Execute(fn)`:
// 1. Immediately when the loop starts.
// 2. After the `interval` duration elapses.
// 3. Immediately when a signal is received on `triggerChan`.
// 4. Stops when the `ctx` context is cancelled.
//
// Parameters:
//   - ctx: Context for cancellation. The loop terminates when ctx is done.
//   - interval: The duration between scheduled executions when the circuit allows.
//   - triggerChan: A channel used to force immediate execution attempts (e.g., via `group.ForceUpdate`). Reads are non-blocking.
//   - fn: The function to execute in each cycle.
//   - errHandling: A callback function invoked when `Execute(fn)` returns an error.
//     Use this for logging or custom error handling specific to the loop runner.
//     Note: `ErrCircuitOpen` will also be passed here when calls are blocked.
func (rm *Routine) Loop(ctx context.Context, interval time.Duration, triggerChan <-chan struct{}, fn func(ctx context.Context) error, errHandling func(err error)) {
	var lastErr error
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := rm.Execute(ctx, fn)
		lastErr = err
		if err != nil {
			errHandling(err)
		}
		select {
		case <-ctx.Done():
			// log.Println("Loop exiting due to context cancellation")
			return
		case <-triggerChan:
			if lastErr != nil {
				time.Sleep(interval)
			}
			// log.Println("Trigger received, executing immediately")
		case <-ticker.C:
			// log.Println("Interval elapsed, executing next cycle")
		}
	}
}

// ForceOpen sets the circuit breaker to the Open state.
func (rm *Routine) ForceOpen() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	log.Println("Forcing circuit breaker to Open state")
	rm.state = Open
	rm.lastFailureAt = time.Now()
	rm.failureCount = rm.threshold
	rm.inTest = false
}

// ForceClose manually sets the circuit breaker state to Closed and resets the
// failure count and test flag.
// Use primarily for testing or manual operational intervention.
func (rm *Routine) ForceClose() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	log.Println("Forcing circuit breaker to Closed state")
	rm.state = Closed
	rm.failureCount = 0
	rm.inTest = false
}

// GetState returns the current State (Closed, Open, HalfOpen) of the circuit breaker.
func (rm *Routine) GetState() State {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.state
}

// GetThreshold returns the failure threshold configured for this circuit breaker.
func (rm *Routine) GetThreshold() int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.threshold
}

// GetResetTimeout returns the reset timeout duration configured for this circuit breaker.
func (rm *Routine) GetResetTimeout() time.Duration {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.resetTimeout
}

// ResetRoutine forces the circuit breaker associated with the given key into the
// 'Closed' state, resetting any tracked failures.
// This is useful for manual intervention or resetting state during tests.
// If no routine manager exists for the key, this function does nothing.
func (p *group) ResetRoutine(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if manager, exists := p.managers[key]; exists {
		manager.ForceClose()
	}
}
