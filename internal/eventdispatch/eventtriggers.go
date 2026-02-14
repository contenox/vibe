package eventdispatch

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/functionstore"
	"github.com/contenox/vibe/libtracker"
)

type TriggerManager interface {
	Trigger
	Sync
}

// Trigger defines the interface for handling events and executing associated functions.
type Trigger interface {
	// HandleEvent processes one or more events and triggers any associated functions.
	HandleEvent(ctx context.Context, events ...*eventstore.Event)
}

type Sync interface {
	Sync(ctx context.Context) error
}

// Executor defines the interface for executing functions with an event as input.
type Executor interface {
	// ExecuteFunction executes a function with the given code and event.
	// It returns a result as a JSON-like map and any error encountered.
	ExecuteFunction(
		ctx context.Context,
		code string,
		functionName string,
		event *eventstore.Event,
	) (map[string]interface{}, error)
}

// FunctionsHandler manages the caching and execution of functions triggered by events.
// It maintains caches of functions and event triggers that are periodically synchronized.
type FunctionsHandler struct {
	functionCache     atomic.Pointer[map[string]*functionstore.Function]
	triggerCache      atomic.Pointer[map[string][]*functionstore.EventTrigger]
	lastFunctionsSync atomic.Int64 // Unix nanoseconds
	lastTriggersSync  atomic.Int64 // Unix nanoseconds
	callInitialSync   atomic.Bool
	syncInterval      time.Duration
	functions         functionservice.Service
	onError           func(context.Context, error)
	tracker           libtracker.ActivityTracker
	triggersInUpdate  atomic.Bool
	functionsInUpdate atomic.Bool
	fnexec            Executor
}

// Sync implements TriggerManager.
func (r *FunctionsHandler) Sync(ctx context.Context) error {
	_, err := r.syncFunctions(ctx)
	if err != nil {
		return err
	}
	_, err = r.syncTriggers(ctx)
	if err != nil {
		return err
	}
	return nil
}

// New creates a new FunctionsHandler instance with initial synchronization.
// It returns a Trigger implementation that can handle events.
//
// Parameters:
//   - ctx: Context for the initialization operations
//   - functions: Service for retrieving functions and triggers
//   - onError: Error handler callback
//   - syncInterval: How often to synchronize with the function service
//   - tracker: Activity tracker for monitoring (optional)
func New(
	ctx context.Context,
	functions functionservice.Service,
	onError func(context.Context, error),
	syncInterval time.Duration,
	fnexec Executor,
	tracker libtracker.ActivityTracker) (TriggerManager, error) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	repo := &FunctionsHandler{
		functions:         functions,
		onError:           onError,
		syncInterval:      syncInterval,
		tracker:           tracker,
		callInitialSync:   atomic.Bool{},
		triggersInUpdate:  atomic.Bool{},
		functionsInUpdate: atomic.Bool{},
		fnexec:            fnexec,
	}

	// Initialize with empty maps
	fc := make(map[string]*functionstore.Function)
	repo.functionCache.Store(&fc)
	tc := make(map[string][]*functionstore.EventTrigger)
	repo.triggerCache.Store(&tc)

	// Perform initial sync
	repo.callInitialSync.Store(true)
	if _, err := repo.syncFunctions(ctx); err != nil {
		return nil, err
	}
	if _, err := repo.syncTriggers(ctx); err != nil {
		return nil, err
	}
	repo.callInitialSync.Store(false)

	return repo, nil
}

// syncFunctions synchronizes the function cache with the function service.
// It only performs I/O operations when necessary and uses atomic flags to prevent redundant operations.
func (r *FunctionsHandler) syncFunctions(ctx context.Context) (map[string]*functionstore.Function, error) {
	// Track the sync operation
	reportErr, reportChange, end := r.tracker.Start(ctx, "sync", "functions_cache")
	defer end()

	// Check if we need to sync
	lastSync := time.Unix(0, r.lastFunctionsSync.Load())
	needSync := r.callInitialSync.Load() || time.Since(lastSync) > r.syncInterval

	if needSync && r.functionsInUpdate.CompareAndSwap(false, true) {
		defer r.functionsInUpdate.Store(false)

		functions, err := r.functions.ListAllFunctions(ctx)
		if err != nil {
			reportErr(err)
			return nil, err
		}

		functionCache := make(map[string]*functionstore.Function)
		for _, f := range functions {
			functionCache[f.Name] = f
		}

		r.functionCache.Store(&functionCache)
		r.lastFunctionsSync.Store(time.Now().UnixNano())
		r.callInitialSync.Store(false)

		// Report successful sync with count
		reportChange("functions_synced", map[string]interface{}{
			"count": len(functionCache),
		})

		return functionCache, nil
	}

	// If no sync needed or sync in progress, return current cache
	return *r.functionCache.Load(), nil
}

// syncTriggers synchronizes the trigger cache with the function service.
// It only performs I/O operations when necessary and uses atomic flags to prevent redundant operations.
func (r *FunctionsHandler) syncTriggers(ctx context.Context) (map[string][]*functionstore.EventTrigger, error) {
	// Track the sync operation
	reportErr, reportChange, end := r.tracker.Start(ctx, "sync", "triggers_cache")
	defer end()

	// Check if a sync is needed and try to acquire the non-blocking lock
	lastSync := time.Unix(0, r.lastTriggersSync.Load())
	needSync := r.callInitialSync.Load() || time.Since(lastSync) > r.syncInterval

	if needSync && r.triggersInUpdate.CompareAndSwap(false, true) {
		defer r.triggersInUpdate.Store(false)

		triggers, err := r.functions.ListAllEventTriggers(ctx)
		if err != nil {
			reportErr(err)
			return nil, err
		}

		// Use a local map to build the new cache with deduplication
		triggerCache := make(map[string][]*functionstore.EventTrigger)
		seenTriggers := make(map[string]map[string]bool) // eventType -> functionName -> exists

		for _, t := range triggers {
			if t == nil {
				continue
			}

			eventType := t.ListenFor.Type
			functionName := t.Function

			// Initialize the inner map if needed
			if _, exists := seenTriggers[eventType]; !exists {
				seenTriggers[eventType] = make(map[string]bool)
			}

			// Deduplicate by function name within the same event type
			if !seenTriggers[eventType][functionName] {
				triggerCache[eventType] = append(triggerCache[eventType], t)
				seenTriggers[eventType][functionName] = true
			}
		}

		r.triggerCache.Store(&triggerCache)
		r.lastTriggersSync.Store(time.Now().UnixNano())
		r.callInitialSync.Store(false)

		// Report successful sync with count
		reportChange("triggers_synced", map[string]interface{}{
			"count":              len(triggers),
			"unique_event_types": len(triggerCache),
		})

		return triggerCache, nil
	}

	// If a sync isn't needed or one is in progress, return the existing cache
	return *r.triggerCache.Load(), nil
}

// FunctionWithTrigger represents a function and its associated trigger.
type FunctionWithTrigger struct {
	Function *functionstore.Function
	Trigger  *functionstore.EventTrigger
}

// GetFunctions retrieves all functions associated with the given event types.
// It returns a mapping from event type to a list of function-trigger pairs.
func (r *FunctionsHandler) GetFunctions(ctx context.Context, eventTypes ...string) (map[string][]*FunctionWithTrigger, error) {
	functionsCache, err := r.syncFunctions(ctx)
	if err != nil {
		return nil, err
	}

	triggerCache, err := r.syncTriggers(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]*FunctionWithTrigger)

	for _, eventType := range eventTypes {
		triggers, ok := triggerCache[eventType]
		if !ok {
			continue
		}

		for _, trigger := range triggers {
			function, ok := functionsCache[trigger.Function]
			if !ok {
				continue
			}

			result[eventType] = append(result[eventType], &FunctionWithTrigger{
				Function: function,
				Trigger:  trigger,
			})
		}
	}

	return result, nil
}

// HandleEvent processes incoming events and triggers any associated functions.
func (r *FunctionsHandler) HandleEvent(ctx context.Context, events ...*eventstore.Event) {
	// Track the event handling
	reportErr, reportChange, end := r.tracker.Start(ctx, "handle", "event",
		"event_count", len(events))
	defer end()

	eventTypes := make([]string, 0, len(events))
	for _, event := range events {
		eventTypes = append(eventTypes, event.EventType)
	}

	functionsWithTrigger, err := r.GetFunctions(ctx, eventTypes...)
	if err != nil {
		r.onError(ctx, err)
		reportErr(err)
		return
	}

	// Report the functions found for these events
	reportChange("functions_found", map[string]interface{}{
		"event_types": eventTypes,
		"functions":   functionsWithTrigger,
	})

	// Execute the associated functions
	for _, event := range events {
		functionList, exists := functionsWithTrigger[event.EventType]
		if !exists {
			continue
		}

		for _, functionWithTrigger := range functionList {
			// Track individual function execution
			funcReportErr, funcReportChange, funcEnd := r.tracker.Start(ctx, "execute", "function",
				"function_name", functionWithTrigger.Function.Name,
				"event_type", event.EventType,
				"event_id", event.ID)

			// Execute the function using the provided executor
			result, err := r.fnexec.ExecuteFunction(
				ctx,
				functionWithTrigger.Function.Script,
				functionWithTrigger.Function.Name,
				event,
			)

			if err != nil {
				// Report execution error
				funcReportErr(err)
				r.onError(ctx, fmt.Errorf("failed to execute function %s: %w",
					functionWithTrigger.Function.Name, err))
			} else {
				// Report successful execution
				funcReportChange("function_executed", map[string]interface{}{
					"function_name": functionWithTrigger.Function.Name,
					"event_type":    event.EventType,
					"event_id":      event.ID,
					"result":        result,
				})
			}

			funcEnd()
		}
	}
}
