package libroutine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/contenox/vibe/libroutine"
)

func TestUnit_GroupAffinitySingleton(t *testing.T) {
	defer quiet()
	t.Run("should return singleton instance", func(t *testing.T) {
		group1 := libroutine.GetGroup()
		group2 := libroutine.GetGroup()
		if group1 != group2 {
			t.Error("Expected group to be singleton, got different instances")
		}
	})
}

func TestUnit_GroupAffinityStartLoop(t *testing.T) {
	group := libroutine.GetGroup()
	ctx := t.Context()

	t.Run("should create new manager and start loop", func(t *testing.T) {
		key := "test-service"
		var callCount int
		var mu sync.Mutex

		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    2,
			ResetTimeout: 100 * time.Millisecond,
			Interval:     10 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				mu.Lock()
				callCount++
				mu.Unlock()
				return nil
			},
		})

		// Allow some time for the loop to execute.
		time.Sleep(25 * time.Millisecond)

		mu.Lock()
		if callCount < 1 {
			t.Errorf("Expected at least 1 call, got %d", callCount)
		}
		mu.Unlock()

		// Verify loop tracking using the public accessor.
		if !group.IsLoopActive(key) {
			t.Error("Loop should be tracked as active")
		}
	})

	t.Run("should prevent duplicate loops for same key", func(t *testing.T) {
		key := "duplicate-test"
		var callCount int
		var mu sync.Mutex

		// Start first loop.
		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    1,
			ResetTimeout: time.Second,
			Interval:     10 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				mu.Lock()
				callCount++
				mu.Unlock()
				return nil
			},
		})

		// Try to start duplicate loop.
		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    1,
			ResetTimeout: time.Second,
			Interval:     10 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				mu.Lock()
				callCount++
				mu.Unlock()
				return nil
			},
		})

		time.Sleep(25 * time.Millisecond)

		mu.Lock()
		if callCount < 1 {
			t.Errorf("Expected at least 1 call, got %d", callCount)
		}
		// We expect only 1 instance running, so call count should be reasonable
		if callCount > 3 { // Allow some margin for timing variations
			t.Errorf("Expected approximately 2-3 calls, got %d (too many, duplicate loop might be running)", callCount)
		}
		mu.Unlock()
	})

	t.Run("should clean up after context cancellation", func(t *testing.T) {
		key := "cleanup-test"
		localCtx, localCancel := context.WithCancel(ctx)

		group.StartLoop(localCtx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    1,
			ResetTimeout: time.Second,
			Interval:     10 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				return nil
			},
		})

		time.Sleep(10 * time.Millisecond)
		localCancel()

		// Wait for cleanup.
		time.Sleep(50 * time.Millisecond) // Increased to ensure cleanup completes

		if group.IsLoopActive(key) {
			t.Error("Loop should be removed from active tracking")
		}
	})

	t.Run("should handle concurrent StartLoop calls", func(t *testing.T) {
		key := "concurrency-test"
		var wg sync.WaitGroup
		var callCount int
		var mu sync.Mutex

		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				group.StartLoop(ctx, &libroutine.LoopConfig{
					Key:          key,
					Threshold:    1,
					ResetTimeout: time.Second,
					Interval:     10 * time.Millisecond,
					Operation: func(ctx context.Context) error {
						mu.Lock()
						callCount++
						mu.Unlock()
						return nil
					},
				})
			}()
		}

		wg.Wait()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		if callCount < 1 {
			t.Errorf("Expected at least 1 call, got %d", callCount)
		}
		// We expect only one instance running, so call count should be reasonable
		if callCount > 6 { // Allow some margin but not excessive
			t.Errorf("Expected approximately 5-6 calls, got %d (too many, concurrency issue)", callCount)
		}
		mu.Unlock()
	})
}

func TestUnit_GroupAffinityCircuitBreaking(t *testing.T) {
	defer quiet()
	group := libroutine.GetGroup()
	ctx := context.Background()

	t.Run("should enforce circuit breaker parameters", func(t *testing.T) {
		key := "circuit-params-test"
		failureThreshold := 3
		resetTimeout := 100 * time.Millisecond

		var failures int
		var mu sync.Mutex

		// Use a function that always fails to ensure circuit stays open
		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    failureThreshold,
			ResetTimeout: resetTimeout,
			Interval:     10 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				mu.Lock()
				failures++
				mu.Unlock()
				return fmt.Errorf("simulated failure")
			},
		})

		// Wait for failures to accumulate and circuit to open
		time.Sleep(50 * time.Millisecond)

		manager := group.GetManager(key)
		if manager == nil {
			t.Fatal("Manager not created")
		}

		// Check if circuit is open
		if manager.GetState() != libroutine.Open {
			t.Errorf("Expected circuit to be open after failures, got state %v", manager.GetState())
		}

		// Wait for reset timeout — then poll for HalfOpen with FAST polling
		timeout := time.After(resetTimeout + 500*time.Millisecond)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		halfOpenObserved := false
		for {
			select {
			case <-timeout:
				t.Fatal("Timeout waiting for HalfOpen state")
			case <-ticker.C:
				if manager.GetState() == libroutine.HalfOpen {
					halfOpenObserved = true
					goto success
				}
			}
		}
	success:
		if !halfOpenObserved {
			t.Error("Expected to observe HalfOpen state, but did not")
		}
	})
}

func TestUnit_GroupAffinityParameterPersistence(t *testing.T) {
	defer quiet()
	group := libroutine.GetGroup()
	ctx := context.Background() // Using Background instead of t.Context() for compatibility

	t.Run("should persist initial parameters", func(t *testing.T) {
		key := "param-persistence-test"
		initialThreshold := 2
		initialTimeout := 100 * time.Millisecond

		// First call with initial parameters.
		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    initialThreshold,
			ResetTimeout: initialTimeout,
			Interval:     10 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				return nil
			},
		})

		// Subsequent call with different parameters.
		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    5,
			ResetTimeout: 200 * time.Millisecond,
			Interval:     20 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				return nil
			},
		})

		manager := group.GetManager(key)
		if manager == nil {
			t.Fatal("Manager not created")
		}

		if manager.GetThreshold() != initialThreshold {
			t.Errorf("Expected threshold %d, got %d", initialThreshold, manager.GetThreshold())
		}
		if manager.GetResetTimeout() != initialTimeout {
			t.Errorf("Expected timeout %v, got %v", initialTimeout, manager.GetResetTimeout())
		}
	})
}

// TestUnit_GroupAffinityResetRoutine verifies we can reset the circuit breaker state
func TestUnit_GroupAffinityResetRoutine(t *testing.T) {
	defer quiet()
	group := libroutine.GetGroup()
	ctx := context.Background()

	t.Run("should reset routine state", func(t *testing.T) {
		key := "reset-routine-test"
		var runCount int
		var mu sync.Mutex
		failureOccurred := make(chan bool, 1)

		// Start a loop that fails once then succeeds
		group.StartLoop(ctx, &libroutine.LoopConfig{
			Key:          key,
			Threshold:    1,
			ResetTimeout: 50 * time.Millisecond,
			Interval:     5 * time.Millisecond,
			Operation: func(ctx context.Context) error {
				mu.Lock()
				runCount++
				currentCount := runCount
				mu.Unlock()

				// Fail only on first execution
				if currentCount == 1 {
					select {
					case failureOccurred <- true:
					default:
					}
					return errors.New("fail once")
				}
				return nil
			},
		})

		// Wait for first failure to occur
		select {
		case <-failureOccurred:
			// Continue with the test
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Timeout waiting for failure to occur")
		}

		manager := group.GetManager(key)
		if manager == nil {
			t.Fatalf("Manager for key %s not found", key)
		}

		// Verify circuit is open after failure
		if manager.GetState() != libroutine.Open {
			t.Fatalf("Expected circuit to be open after failure, got %v", manager.GetState())
		}

		// Wait for reset timeout — then poll for HalfOpen
		timeout := time.After(500 * time.Millisecond)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		halfOpenObserved := false
		for {
			select {
			case <-timeout:
				t.Fatal("Timeout waiting for HalfOpen state")
			case <-ticker.C:
				if manager.GetState() == libroutine.HalfOpen {
					halfOpenObserved = true
					goto afterHalfOpen
				}
			}
		}
	afterHalfOpen:
		if !halfOpenObserved {
			t.Error("Expected to observe HalfOpen state, but did not")
		}

		// Now force update to trigger the successful call and transition to Closed
		group.ForceUpdate(key)
		time.Sleep(25 * time.Millisecond)

		// Verify circuit is now closed
		if manager.GetState() != libroutine.Closed {
			t.Errorf("Expected manager state to be Closed after successful call, got %v", manager.GetState())
		}
	})
}
