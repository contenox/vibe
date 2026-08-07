package libroutine

import (
	"context"
	"log"
	"sync"
	"time"
)

// group provides a centralized way to manage and run keyed background routines.
// It ensures that for any given key, only one instance of the associated routine's
// loop is active at a time.
// Access to the group is done via the singleton instance returned by Getgroup.
type group struct {
	managers   map[string]*Routine      // Maps keys to Routine instances
	loops      map[string]bool          // Tracks whether a loop is active for a key
	triggerChs map[string]chan struct{} // Per-key trigger channels for forcing an update
	mu         sync.Mutex               // Protects access to maps
}

var (
	groupInstance *group
	groupOnce     sync.Once
)

// GetGroup returns the singleton instance of the group.
func GetGroup() *group {
	groupOnce.Do(func() {
		log.Println("Initializing routine group")
		groupInstance = &group{
			managers:   make(map[string]*Routine),
			loops:      make(map[string]bool),
			triggerChs: make(map[string]chan struct{}),
		}
	})
	return groupInstance
}

type LoopConfig struct {
	Key          string                          // A unique string identifier for this routine. Used to prevent duplicates and manage state.
	Threshold    int                             // The number of consecutive failures of `fn` before the circuit breaker opens.
	ResetTimeout time.Duration                   // The duration the circuit breaker stays open before transitioning to half-open.
	Interval     time.Duration                   // The time duration between executions of `fn` when the circuit is closed or half-open (and the attempt succeeds).
	Operation    func(ctx context.Context) error // The function to execute periodically. It receives the context and should return an error on failure
}

// StartLoop initiates and manages a background loop for a specific task identified by `key`.
//
// The loop repeatedly executes the provided function `fn` at the specified `interval`.
// Execution is wrapped by a circuit breaker (`Routine`) configured with `threshold`
// (number of failures to trip) and `resetTimeout` (duration to wait before trying again
// in HalfOpen state).
//
// If a loop for the given `key` is already running, this function does nothing.
// The loop respects the `ctx` context for cancellation. If the context is cancelled,
// the loop will terminate gracefully.
//
// Params:
//   - ctx: Context for managing the loop's lifecycle. Cancellation stops the loop.
//   - cfg: Configuration for the loop.
//
// Example:
//
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel() // Ensure cleanup
//	group := routine.Getgroup()
//	group.StartLoop(
//	    ctx,
//	    &LoopConfig{
//	        Key: "my-data-processor",
//	        Threshold: 3, // Open circuit after 3 failures
//	        ResetTimeout: 1*time.Minute, // Wait 1 minute before trying again
//	        Interval: 10*time.Second, // Run every 10 seconds
//	        Fn: func(ctx context.Context) error {
//	            log.Println("Processing data...")
//	            // ... perform task ...
//	            // return errors.New("failed to process data") // On failure
//	            return nil // On success
//	        },
//	    },
//	)
//	// The loop now runs in the background.
func (p *group) StartLoop(ctx context.Context, cfg *LoopConfig) {
	p.mu.Lock()
	log.Printf("Starting loop for key: %s", cfg.Key)
	defer p.mu.Unlock()

	// Create a new Routine if none exists for the key.
	manager, exists := p.managers[cfg.Key]
	if !exists {
		log.Printf("Creating new routine manager for key: %s", cfg.Key)
		manager = NewRoutine(cfg.Threshold, cfg.ResetTimeout)
		p.managers[cfg.Key] = manager
	}

	// If a loop for this key is already active, do nothing.
	if p.loops[cfg.Key] {
		log.Printf("Loop for key %s is already active", cfg.Key)
		return
	}

	// Create a new trigger channel for this loop.
	triggerChan := make(chan struct{}, 1)
	p.triggerChs[cfg.Key] = triggerChan

	// Mark the loop as active.
	p.loops[cfg.Key] = true

	// Start the loop in a new goroutine. manager is captured here, while
	// still holding p.mu, rather than re-read from p.managers[cfg.Key]
	// inside the goroutine — that read would race against another
	// StartLoop's concurrent write to the same map.
	go func() {
		log.Printf("Loop started for key: %s", cfg.Key)
		manager.Loop(ctx, cfg.Interval, triggerChan, cfg.Operation, func(err error) {
			if err != nil {
				log.Printf("Error in loop for key %s: %v", cfg.Key, err)
			}
		})
		// Clean up when the loop exits.
		p.mu.Lock()
		delete(p.loops, cfg.Key)
		delete(p.triggerChs, cfg.Key)
		p.mu.Unlock()
		log.Printf("Loop stopped for key: %s", cfg.Key)
	}()
}

// IsLoopActive checks if a background loop associated with the given key is
// currently marked as active within the group.
// This is primarily intended for testing or monitoring purposes.
// Returns true if a loop is active, false otherwise.
func (p *group) IsLoopActive(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loops[key]
}

// ForceUpdate triggers an immediate execution attempt for the loop associated with the key,
// bypassing the regular interval timer.
// If the loop's circuit breaker is 'Open', this trigger will still be blocked
// until the breaker transitions to 'HalfOpen'.
// If no loop is active for the key, or if an update is already pending (channel is full),
// this call has no effect.
func (p *group) ForceUpdate(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	log.Printf("Forcing update for key: %s", key)
	if triggerChan, ok := p.triggerChs[key]; ok {
		select {
		case triggerChan <- struct{}{}:
			log.Printf("Update triggered for key: %s", key)
		default:
			log.Printf("Update already pending for key: %s", key)
		}
	}
}

// GetManager exposes the Routine associated with a key for testing.
func (p *group) GetManager(key string) *Routine {
	p.mu.Lock()
	defer p.mu.Unlock()
	log.Printf("Retrieving manager for key: %s", key)
	return p.managers[key]
}
