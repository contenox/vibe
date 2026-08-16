package libroutine

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// State represents the operational state of the Routine (circuit breaker).
type State int

const (
	// Closed allows operations to execute and counts failures.
	Closed State = iota
	// Open prevents operations from executing, until a timeout moves it to HalfOpen.
	Open
	// HalfOpen allows a single test operation: success closes the circuit, failure reopens it.
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

// ErrCircuitOpen is returned by Execute when the circuit breaker is Open.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Routine is a circuit breaker: it tracks failures, opens the circuit when a
// threshold is reached, and resets automatically after a timeout via HalfOpen.
type Routine struct {
	mu            sync.Mutex
	state         State
	failureCount  int
	threshold     int
	resetTimeout  time.Duration
	lastFailureAt time.Time
	inTest        bool
}

// NewRoutine creates a Routine that opens after threshold consecutive failures
// and stays Open for resetTimeout before transitioning to HalfOpen.
func NewRoutine(threshold int, resetTimeout time.Duration) *Routine {
	return &Routine{
		threshold:    threshold,
		resetTimeout: resetTimeout,
		state:        Closed,
	}
}

// Allow reports whether the circuit breaker permits an operation. It may
// transition the state from Open to HalfOpen if the reset timeout has passed.
func (rm *Routine) Allow() bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	switch rm.state {
	case Closed:
		return true
	case Open:
		if time.Since(rm.lastFailureAt) > rm.resetTimeout {
			rm.state = HalfOpen
			rm.inTest = false
		} else {
			return false
		}
	case HalfOpen:
		if rm.inTest {
			return false
		}
	}

	if rm.state == HalfOpen && !rm.inTest {
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
		rm.state = Closed
		rm.failureCount = 0
		rm.inTest = false
	}
}

// MarkFailure records a failed operation, tripping the circuit to Open once the
// threshold is reached or a HalfOpen test fails.
func (rm *Routine) MarkFailure() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	switch rm.state {
	case Closed:
		rm.failureCount++
		if rm.failureCount >= rm.threshold {
			rm.state = Open
			rm.lastFailureAt = time.Now().UTC()
		}
	case HalfOpen:
		rm.state = Open
		rm.lastFailureAt = time.Now().UTC()
		rm.inTest = false
	}
}

// Execute runs fn if allowed by the circuit breaker, recording the outcome.
func (rm *Routine) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if !rm.Allow() {
		return ErrCircuitOpen
	}

	err := fn(ctx)
	if err != nil {
		rm.MarkFailure()
	} else {
		rm.MarkSuccess()
	}
	return err
}

// ExecuteWithRetry runs fn via Execute, retrying on failure up to iterations
// times with a fixed interval between attempts. It returns the last error, or
// the context cause if ctx is cancelled.
func (rm *Routine) ExecuteWithRetry(ctx context.Context, interval time.Duration, iterations int, fn func(ctx context.Context) error) error {
	var err error
	for range iterations {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err = rm.Execute(ctx, fn); err == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return err
}

// Loop runs fn via Execute immediately, then on every interval tick or
// triggerChan signal, until ctx is cancelled. errHandling is called with each
// error, including ErrCircuitOpen.
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
			return
		case <-triggerChan:
			if lastErr != nil {
				time.Sleep(interval)
			}
		case <-ticker.C:
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

// ForceClose sets the circuit breaker to Closed and resets the failure count.
func (rm *Routine) ForceClose() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	log.Println("Forcing circuit breaker to Closed state")
	rm.state = Closed
	rm.failureCount = 0
	rm.inTest = false
}

// GetState returns the current State of the circuit breaker.
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

// ResetRoutine forces the circuit breaker for key into the Closed state. It does
// nothing if no routine exists for the key.
func (p *group) ResetRoutine(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if manager, exists := p.managers[key]; exists {
		manager.ForceClose()
	}
}
