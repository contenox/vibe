package libroutine

import (
	"context"
	"log"
	"sync"
	"time"
)

// group manages keyed background routines, running at most one loop per key.
type group struct {
	managers   map[string]*Routine
	loops      map[string]bool
	triggerChs map[string]chan struct{}
	mu         sync.Mutex
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

// LoopConfig configures one managed background loop.
type LoopConfig struct {
	// Key uniquely identifies the routine and prevents duplicate loops.
	Key string
	// Threshold is the number of consecutive failures before the circuit opens.
	Threshold int
	// ResetTimeout is how long the circuit stays open before half-open.
	ResetTimeout time.Duration
	// Interval is the time between executions.
	Interval time.Duration
	// Operation is the function executed periodically.
	Operation func(ctx context.Context) error
}

// StartLoop starts a background loop for cfg.Key, wrapped by a circuit breaker.
// It does nothing if a loop for that key is already running, and terminates when
// ctx is cancelled.
func (p *group) StartLoop(ctx context.Context, cfg *LoopConfig) {
	p.mu.Lock()
	log.Printf("Starting loop for key: %s", cfg.Key)
	defer p.mu.Unlock()

	manager, exists := p.managers[cfg.Key]
	if !exists {
		log.Printf("Creating new routine manager for key: %s", cfg.Key)
		manager = NewRoutine(cfg.Threshold, cfg.ResetTimeout)
		p.managers[cfg.Key] = manager
	}

	if p.loops[cfg.Key] {
		log.Printf("Loop for key %s is already active", cfg.Key)
		return
	}

	triggerChan := make(chan struct{}, 1)
	p.triggerChs[cfg.Key] = triggerChan

	p.loops[cfg.Key] = true

	// manager is captured under p.mu; re-reading the map inside the goroutine
	// would race a concurrent StartLoop.
	go func() {
		log.Printf("Loop started for key: %s", cfg.Key)
		manager.Loop(ctx, cfg.Interval, triggerChan, cfg.Operation, func(err error) {
			if err != nil {
				log.Printf("Error in loop for key %s: %v", cfg.Key, err)
			}
		})
		p.mu.Lock()
		delete(p.loops, cfg.Key)
		delete(p.triggerChs, cfg.Key)
		p.mu.Unlock()
		log.Printf("Loop stopped for key: %s", cfg.Key)
	}()
}

// IsLoopActive reports whether a background loop for key is currently active.
func (p *group) IsLoopActive(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loops[key]
}

// ForceUpdate triggers an immediate execution attempt for key, bypassing the
// interval timer. It has no effect when no loop is active or an update is
// already pending.
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
