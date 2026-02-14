package libroutine_test

import (
	"context"
	"errors"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/vibe/libroutine"
)

func quiet() func() {
	null, _ := os.Open(os.DevNull)
	sout := os.Stdout
	serr := os.Stderr
	os.Stdout = null
	os.Stderr = null
	log.SetOutput(null)
	return func() {
		defer null.Close()
		os.Stdout = sout
		os.Stderr = serr
		log.SetOutput(os.Stderr)
	}
}

func TestCircuitBreaker_ClosedState_AllowsExecution(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(3, time.Second)

	if !rm.Allow() {
		t.Errorf("expected Allow to return true in closed state")
	}

	err := rm.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})

	if err != nil {
		t.Errorf("expected Execute to succeed, got error: %v", err)
	}
}

func TestCircuitBreaker_OpensAfterFailures(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, 500*time.Millisecond)

	err := rm.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("test error")
	})

	if err == nil {
		t.Errorf("expected Execute to return an error")
	}

	if rm.Allow() {
		t.Errorf("expected Allow to return false after failure threshold exceeded")
	}
}

func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, 200*time.Millisecond)

	// Cause the circuit to open
	_ = rm.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("test error")
	})

	// Wait for reset timeout (use polling instead of sleep)
	deadline := time.Now().Add(202 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rm.Allow() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// First call in half-open should be allowed
	if !rm.Allow() {
		t.Errorf("expected Allow to return true in half-open state")
	}

	// Second call in half-open should be blocked
	if rm.Allow() {
		t.Errorf("expected Allow to return false in half-open state when test call is in progress")
	}
}

func TestCircuitBreaker_RecoversFromHalfOpenOnSuccess(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, 200*time.Millisecond)

	// Cause the circuit to open
	_ = rm.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("test error")
	})

	// Wait for reset timeout
	time.Sleep(250 * time.Millisecond)

	// First call in half-open should be allowed and succeed
	err := rm.Execute(context.Background(), func(ctx context.Context) error {
		return nil
	})

	if err != nil {
		t.Errorf("expected Execute to succeed in half-open state, got error: %v", err)
	}

	// Ensure further calls are allowed (circuit should be fully closed again)
	if !rm.Allow() {
		t.Errorf("expected Allow to return true after recovering from half-open state")
	}
}

func TestCircuitBreaker_ReopensAfterFailureInHalfOpen(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, 200*time.Millisecond)

	// Cause the circuit to open
	_ = rm.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("test error")
	})

	// Wait for reset timeout
	time.Sleep(250 * time.Millisecond)

	// First call in half-open should be allowed but fail
	_ = rm.Execute(context.Background(), func(ctx context.Context) error {
		return errors.New("test error")
	})

	// Circuit should now be open again, blocking calls
	if rm.Allow() {
		t.Errorf("expected Allow to return false after failure in half-open state")
	}
}

func TestCircuitBreaker_LoopExecutesFunction(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	triggerChan := make(chan struct{})
	callCount := 0
	fn := func(ctx context.Context) error {
		callCount++
		return nil
	}

	// Start the loop in a separate goroutine.
	go rm.Loop(ctx, 100*time.Millisecond, triggerChan, fn, func(err error) {})

	// Let the loop run for a short while.
	time.Sleep(350 * time.Millisecond)
	cancel()

	// Give a moment for the loop to exit.
	time.Sleep(150 * time.Millisecond)

	if callCount < 2 {
		t.Errorf("expected loop to execute at least 2 calls, got %d", callCount)
	}
}
func TestCircuitBreaker_GetState(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(2, time.Second)

	if rm.GetState() != libroutine.Closed {
		t.Errorf("expected initial state to be Closed, got %v", rm.GetState())
	}

	// Force the state to Open and check
	rm.ForceOpen()
	if rm.GetState() != libroutine.Open {
		t.Errorf("expected state to be Open after ForceOpen, got %v", rm.GetState())
	}

	// Force the state to Closed and check
	rm.ForceClose()
	if rm.GetState() != libroutine.Closed {
		t.Errorf("expected state to be Closed after ForceClose, got %v", rm.GetState())
	}
}

func TestCircuitBreaker_GetThreshold(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(3, time.Second)

	if rm.GetThreshold() != 3 {
		t.Errorf("expected threshold to be 3, got %d", rm.GetThreshold())
	}
}

func TestCircuitBreaker_GetResetTimeout(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(3, 2*time.Second)

	if rm.GetResetTimeout() != 2*time.Second {
		t.Errorf("expected reset timeout to be 2 seconds, got %v", rm.GetResetTimeout())
	}
}
func TestCircuitBreaker_ForceOpen(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(2, time.Second)

	rm.ForceOpen()
	if rm.GetState() != libroutine.Open {
		t.Errorf("expected state to be Open after ForceOpen, got %v", rm.GetState())
	}

	if rm.Allow() {
		t.Errorf("expected Allow to return false after ForceOpen")
	}
}

func TestCircuitBreaker_ForceClose(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(2, time.Second)

	// Force the circuit to open
	rm.ForceOpen()
	rm.ForceClose()

	if rm.GetState() != libroutine.Closed {
		t.Errorf("expected state to be Closed after ForceClose, got %v", rm.GetState())
	}

	if !rm.Allow() {
		t.Errorf("expected Allow to return true after ForceClose")
	}
}

// TestRoutine_Execute_ReturnsErrCircuitOpen specifically verifies that Execute
// returns the correct error type when the circuit is open.
func TestRoutine_Execute_ReturnsErrCircuitOpen(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, time.Minute) // Long timeout

	// Force open
	rm.ForceOpen()

	err := rm.Execute(context.Background(), func(ctx context.Context) error {
		t.Error("Function should not have been executed when circuit is open")
		return nil
	})

	if !errors.Is(err, libroutine.ErrCircuitOpen) {
		t.Errorf("Expected error to be ErrCircuitOpen, got %v", err)
	}
}

// TestSuite for ExecuteWithRetry
func TestRoutine_ExecuteWithRetry(t *testing.T) {
	defer quiet()()
	t.Run("SuccessFirstTry", func(t *testing.T) {
		rm := libroutine.NewRoutine(1, time.Minute)
		var callCount int32
		fn := func(ctx context.Context) error {
			atomic.AddInt32(&callCount, 1)
			return nil
		}
		err := rm.ExecuteWithRetry(context.Background(), 10*time.Millisecond, 3, fn)
		if err != nil {
			t.Errorf("Expected success, got error: %v", err)
		}
		if atomic.LoadInt32(&callCount) != 1 {
			t.Errorf("Expected function to be called 1 time, got %d", atomic.LoadInt32(&callCount))
		}
	})

	t.Run("SuccessAfterRetry", func(t *testing.T) {
		rm := libroutine.NewRoutine(5, time.Minute) // High threshold to prevent opening
		var callCount int32
		testErr := errors.New("retry error")
		fn := func(ctx context.Context) error {
			count := atomic.AddInt32(&callCount, 1)
			if count < 3 {
				return testErr
			}
			return nil // Success on 3rd try
		}
		err := rm.ExecuteWithRetry(context.Background(), 10*time.Millisecond, 5, fn) // Allow 5 attempts
		if err != nil {
			t.Errorf("Expected success after retries, got error: %v", err)
		}
		if atomic.LoadInt32(&callCount) != 3 {
			t.Errorf("Expected function to be called 3 times, got %d", atomic.LoadInt32(&callCount))
		}
	})

	t.Run("FailureAllRetries", func(t *testing.T) {
		rm := libroutine.NewRoutine(5, time.Minute) // High threshold
		var callCount int32
		testErr := errors.New("persistent error")
		fn := func(ctx context.Context) error {
			atomic.AddInt32(&callCount, 1)
			return testErr
		}
		err := rm.ExecuteWithRetry(context.Background(), 10*time.Millisecond, 3, fn) // 3 attempts
		if !errors.Is(err, testErr) {
			t.Errorf("Expected persistent error %v, got %v", testErr, err)
		}
		if atomic.LoadInt32(&callCount) != 3 {
			t.Errorf("Expected function to be called 3 times, got %d", atomic.LoadInt32(&callCount))
		}
	})

	t.Run("FailureCircuitOpen", func(t *testing.T) {
		rm := libroutine.NewRoutine(1, time.Minute) // Low threshold
		var callCount int32
		fn := func(ctx context.Context) error {
			atomic.AddInt32(&callCount, 1)
			return errors.New("failure") // Fail immediately
		}
		// First call opens the circuit
		_ = rm.Execute(context.Background(), fn)
		if rm.GetState() != libroutine.Open {
			t.Fatalf("Circuit should be open")
		}

		// Now try ExecuteWithRetry
		atomic.StoreInt32(&callCount, 0) // Reset counter
		err := rm.ExecuteWithRetry(context.Background(), 10*time.Millisecond, 3, fn)

		// It should immediately return ErrCircuitOpen without calling fn again
		if !errors.Is(err, libroutine.ErrCircuitOpen) {
			t.Errorf("Expected ErrCircuitOpen when circuit is already open, got %v", err)
		}
		if atomic.LoadInt32(&callCount) != 0 {
			t.Errorf("Expected function not to be called when circuit is open, called %d times", atomic.LoadInt32(&callCount))
		}
	})

	t.Run("ContextCancelledDuringSleep", func(t *testing.T) {
		rm := libroutine.NewRoutine(5, time.Minute)
		var callCount int32
		testErr := errors.New("fail first")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fn := func(innerCtx context.Context) error {
			count := atomic.AddInt32(&callCount, 1)
			if count == 1 {
				// Cancel context *after* the first call fails, during the sleep
				go func() {
					time.Sleep(5 * time.Millisecond) // Give time for ExecuteWithRetry to start sleeping
					cancel()
				}()
				return testErr
			}
			t.Errorf("Function should not be called more than once")
			return nil
		}

		err := rm.ExecuteWithRetry(ctx, 50*time.Millisecond, 3, fn) // Longish sleep

		// Check if the context error is returned
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Expected context.Canceled error, got %v", err)
		}
		if atomic.LoadInt32(&callCount) != 1 {
			t.Errorf("Expected function to be called 1 time, got %d", atomic.LoadInt32(&callCount))
		}
	})

	// Note: Testing ContextCancelledDuringExecution is harder without a way
	// for fn to reliably block and detect cancellation *during* its run
	// within the retry framework. This often requires more complex fn mocks.
}

// TestRoutine_Loop_Trigger verifies that the trigger channel causes immediate execution.
func TestRoutine_Loop_Trigger(t *testing.T) {
	defer quiet()()
	rm := libroutine.NewRoutine(1, time.Minute)
	ctx := t.Context()

	triggerChan := make(chan struct{}, 1)
	executedChan := make(chan bool, 2)

	fn := func(ctx context.Context) error {
		select {
		case executedChan <- true:
		default:
		}
		return nil
	}

	// Start loop with a very long interval
	go rm.Loop(ctx, 1*time.Minute, triggerChan, fn, func(err error) {})

	// Wait for the initial execution which happens immediately.
	select {
	case <-executedChan:
		// Drain the first immediate execution.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Initial execution did not occur as expected")
	}

	// Send trigger.
	triggerChan <- struct{}{}

	// Wait for execution signal due to trigger.
	select {
	case <-executedChan:
		// Success! The trigger caused immediate execution.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Function did not execute after trigger within timeout")
	}
}

func TestRoutine_Loop_ErrHandling(t *testing.T) {
	defer quiet()()
	resetTimeout := 200 * time.Millisecond // Short reset timeout
	interval := 20 * time.Millisecond      // Short interval for trigger sleep
	rm := libroutine.NewRoutine(1, resetTimeout)
	ctx := t.Context()

	triggerChan := make(chan struct{}, 1)
	errChan := make(chan error, 2) // Buffer for 2 errors
	testErr := errors.New("loop function error")

	fn := func(ctx context.Context) error {
		return testErr
	}

	go rm.Loop(ctx, interval, triggerChan, fn, func(err error) {
		select {
		case errChan <- err:
		default:
		}
	})

	// 1. Wait for initial run (automatic on loop start)
	select {
	case <-errChan: // Drain initial error
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for initial error")
	}

	// 2. Trigger while circuit is still open (before reset timeout)
	triggerChan <- struct{}{}
	select {
	case err := <-errChan:
		if !errors.Is(err, libroutine.ErrCircuitOpen) {
			t.Errorf("Expected ErrCircuitOpen, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for open state error")
	}
}
