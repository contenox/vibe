package executor

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/contenox/vibe/eventsourceservice"
	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/functionservice"
	"github.com/contenox/vibe/functionstore"
	"github.com/contenox/vibe/internal/eventdispatch"
	"github.com/contenox/vibe/jseval"
	"github.com/contenox/vibe/libroutine"
	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
	"github.com/dop251/goja"
)

// Ensure GojaExecutor implements the Executor interface
var _ eventdispatch.Executor = (*GojaExecutor)(nil)

// GojaExecutor handles JavaScript function execution using goja VM with pre-compiled functions.
// It provides a secure sandboxed environment for executing user-provided JavaScript code
// with access to system services through controlled built-in functions.
type GojaExecutor struct {
	tracker              libtracker.ActivityTracker
	vmPool               sync.Pool
	functionCache        *sync.Map // functionName -> *compiledFunction
	eventsource          eventsourceservice.Service
	taskService          execservice.ExecService
	taskchainService     taskchainservice.Service
	taskchainExecService execservice.TasksEnvService
	functionService      functionservice.Service
	syncRoutine          *libroutine.Routine
	syncTriggerChan      chan struct{}
	syncCancelFunc       context.CancelFunc
	syncWG               sync.WaitGroup
	hookRepo             taskengine.HookRepo

	jsEnv *jseval.Env
}

// compiledFunction represents a pre-compiled JavaScript function with version tracking
type compiledFunction struct {
	program  *goja.Program
	codeHash string
}

// NewGojaExecutor creates a new Goja executor with VM pool and function cache.
//
// Parameters:
//   - tracker: Activity tracker for monitoring execution
//   - functionService: Service for synchronizing function-scripts
//
// Returns:
//   - Executor: A new GojaExecutor instance
func NewGojaExecutor(
	tracker libtracker.ActivityTracker,
	functionService functionservice.Service,
) *GojaExecutor {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}

	executor := &GojaExecutor{
		tracker:         tracker,
		functionCache:   &sync.Map{},
		functionService: functionService,
		syncTriggerChan: make(chan struct{}, 1),
	}

	// Initialize the circuit breaker for sync operations
	executor.syncRoutine = libroutine.NewRoutine(
		3,             // 3 failures before opening circuit
		5*time.Minute, // 5 minutes reset timeout
	)

	// Initialize VM pool - built-ins will be bound per-execution with correct context
	executor.vmPool = sync.Pool{
		New: func() interface{} {
			return goja.New() // No built-ins here - they are bound per request
		},
	}

	return executor
}

func (e *GojaExecutor) AddBuildInServices(
	eventsource eventsourceservice.Service,
	taskService execservice.ExecService,
	taskchainService taskchainservice.Service,
	taskchainExecService execservice.TasksEnvService,
	hookRepo taskengine.HookRepo,
) {
	e.eventsource = eventsource
	e.taskService = taskService
	e.taskchainService = taskchainService
	e.taskchainExecService = taskchainExecService

	// Create a shared JS eval environment with REAL service deps.
	e.jsEnv = jseval.NewEnv(
		e.tracker,
		jseval.BuiltinHandlers{
			Eventsource:          eventsource,
			TaskService:          taskService,
			TaskchainService:     taskchainService,
			TaskchainExecService: taskchainExecService,
			FunctionService:      e.functionService,
			HookRepo:             e.hookRepo,
		},
		jseval.DefaultBuiltins(),
	)
}

// StartSync begins background synchronization of the function cache
// Parameters:
//   - ctx: Context for managing the sync lifecycle
//   - syncInterval: How often to perform automatic syncs
func (e *GojaExecutor) StartSync(ctx context.Context, syncInterval time.Duration) {
	syncCtx, cancel := context.WithCancel(ctx)
	e.syncCancelFunc = cancel

	e.syncWG.Add(1)
	go e.syncLoop(syncCtx, syncInterval)
}

// StopSync stops the background synchronization
func (e *GojaExecutor) StopSync() {
	if e.syncCancelFunc != nil {
		e.syncCancelFunc()
	}
	e.syncWG.Wait()
}

// TriggerSync manually triggers a synchronization
func (e *GojaExecutor) TriggerSync() {
	select {
	case e.syncTriggerChan <- struct{}{}:
		// Trigger sent successfully
	default:
		// Trigger channel is full, sync already pending
	}
}

// syncLoop handles the background synchronization with circuit breaker protection
func (e *GojaExecutor) syncLoop(ctx context.Context, syncInterval time.Duration) {
	defer e.syncWG.Done()

	// Initial sync on startup
	if err := e.syncWithCircuitBreaker(ctx); err != nil {
		reportErr, _, end := e.tracker.Start(ctx, "sync", "function_cache",
			"error", "initial_sync_failed", "err", err.Error())
		defer end()
		reportErr(err)
	}

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Periodic sync
			if err := e.syncWithCircuitBreaker(ctx); err != nil {
				reportErr, _, end := e.tracker.Start(ctx, "sync", "function_cache",
					"error", "periodic_sync_failed", "err", err.Error())
				defer end()
				reportErr(err)
			}
		case <-e.syncTriggerChan:
			// Manual trigger
			if err := e.syncWithCircuitBreaker(ctx); err != nil {
				reportErr, _, end := e.tracker.Start(ctx, "sync", "function_cache",
					"error", "triggered_sync_failed", "err", err.Error())
				defer end()
				reportErr(err)
			}
		}
	}
}

// syncWithCircuitBreaker executes sync with circuit breaker protection
func (e *GojaExecutor) syncWithCircuitBreaker(ctx context.Context) error {
	return e.syncRoutine.Execute(ctx, func(ctx context.Context) error {
		return e.syncFunctionCache(ctx)
	})
}

// ExecuteFunction executes a JavaScript function with the given event as input.
//
// Parameters:
//   - ctx: Execution context
//   - code: JavaScript code to execute
//   - functionName: Name of the function to call (or use global result)
//   - event: Event that triggered the function execution
//
// Returns:
//   - map[string]interface{}: Function result as a JSON-like map
//   - error: Any error encountered during execution
func (e *GojaExecutor) ExecuteFunction(
	ctx context.Context,
	code string,
	functionName string,
	event *eventstore.Event,
) (map[string]interface{}, error) {
	// Start tracking the function execution
	reportErr, reportChange, end := e.tracker.Start(ctx, "execute", "function",
		"function_name", functionName,
		"event_type", event.EventType,
		"event_id", event.ID)
	defer end()

	// Get or compile the function
	compiledFunc, err := e.getCompiledFunction(ctx, functionName, code)
	if err != nil {
		reportErr(fmt.Errorf("failed to get compiled function: %w", err))
		return nil, err
	}

	// Get a VM from the pool
	vm := e.vmPool.Get().(*goja.Runtime)
	defer e.vmPool.Put(vm)

	// Bind built-in functions with current request context and error handling
	e.setupContextBoundBuiltins(ctx, vm)

	// Reset VM state (clear previous execution results)
	_ = vm.Set("result", nil)

	// Prepare event data for JavaScript
	eventObj, err := e.prepareEventObject(event)
	if err != nil {
		reportErr(fmt.Errorf("failed to prepare event object: %w", err))
		return nil, err
	}

	// Set the event as a global variable
	if err := vm.Set("event", eventObj); err != nil {
		reportErr(fmt.Errorf("failed to set event in VM: %w", err))
		return nil, err
	}

	// Run the pre-compiled program
	if _, err := vm.RunProgram(compiledFunc.program); err != nil {
		reportErr(fmt.Errorf("failed to execute compiled function: %w", err))
		return nil, err
	}

	// Check if the script defines a function with the expected name
	fnVal := vm.Get(functionName)
	if goja.IsUndefined(fnVal) || goja.IsNull(fnVal) {
		// Script doesn't define a function, assume it runs immediately and sets a result
		resultVal := vm.Get("result")
		if goja.IsUndefined(resultVal) || goja.IsNull(resultVal) {
			// No result set, return empty result
			return nil, nil
		}

		// Convert the result to a Go map
		result := make(map[string]interface{})
		if err := vm.ExportTo(resultVal, &result); err != nil {
			reportErr(fmt.Errorf("failed to export result: %w", err))
			return nil, err
		}

		// Validate result is JSON compatible
		if _, err := json.Marshal(result); err != nil {
			reportErr(fmt.Errorf("function returned invalid JSON: %w", err))
			return nil, err
		}

		// Report successful execution with change
		reportChange(functionName, map[string]interface{}{
			"event_type": event.EventType,
			"event_id":   event.ID,
			"result":     result,
		})

		return result, nil
	}

	// Script defines a function, so call it
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		err := fmt.Errorf("%s is not a function", functionName)
		reportErr(err)
		return nil, err
	}

	// Call the function with event as argument
	resultVal, err := fn(goja.Undefined(), vm.ToValue(eventObj))
	if err != nil {
		reportErr(fmt.Errorf("failed to call function: %w", err))
		return nil, err
	}

	// Handle undefined/null results
	if goja.IsNull(resultVal) || goja.IsUndefined(resultVal) {
		return nil, nil
	}

	// Convert the result to a Go map
	result := make(map[string]interface{})
	if err := vm.ExportTo(resultVal, &result); err != nil {
		reportErr(fmt.Errorf("failed to export result: %w", err))
		return nil, err
	}

	// Validate result is JSON compatible
	if _, err := json.Marshal(result); err != nil {
		reportErr(fmt.Errorf("function returned invalid JSON: %w", err))
		return nil, err
	}

	// Report successful execution with change
	reportChange(functionName, map[string]interface{}{
		"event_type": event.EventType,
		"event_id":   event.ID,
		"result":     result,
	})

	return result, nil
}

// getCompiledFunction retrieves or compiles a function, using caching for performance.
//
// Parameters:
//   - ctx: Execution context
//   - functionName: Name of the function
//   - code: JavaScript code to compile
//
// Returns:
//   - *compiledFunction: The compiled function
//   - error: Any error encountered during compilation
func (e *GojaExecutor) getCompiledFunction(ctx context.Context, functionName, code string) (*compiledFunction, error) {
	// Generate hash for code versioning
	hash := sha1.Sum([]byte(code))
	codeHash := base64.StdEncoding.EncodeToString(hash[:])

	// Check if we have a cached version
	if cached, ok := e.functionCache.Load(functionName); ok {
		compiled := cached.(*compiledFunction)
		if compiled.codeHash == codeHash {
			return compiled, nil
		}
		// Track function recompilation due to code change
		reportErr, _, end := e.tracker.Start(ctx, "recompile", "function",
			"function_name", functionName,
			"reason", "code_changed")

		// Remove old version from cache
		e.functionCache.Delete(functionName)
		end()
		reportErr(nil)
	}

	// Track function compilation
	reportErr, reportChange, end := e.tracker.Start(ctx, "compile", "function",
		"function_name", functionName)
	defer end()

	// Compile new function
	program, err := goja.Compile(functionName, code, false)
	if err != nil {
		reportErr(fmt.Errorf("failed to compile function %s: %w", functionName, err))
		return nil, err
	}

	compiled := &compiledFunction{
		program:  program,
		codeHash: codeHash,
	}

	// Cache the compiled function
	e.functionCache.Store(functionName, compiled)

	// Report successful compilation
	reportChange(functionName, map[string]interface{}{
		"code_hash": codeHash,
	})

	return compiled, nil
}

// prepareEventObject converts an event to a format suitable for JavaScript.
//
// Parameters:
//   - event: The event to convert
//
// Returns:
//   - map[string]interface{}: JavaScript-compatible event object
//   - error: Any error encountered during conversion
func (e *GojaExecutor) prepareEventObject(event *eventstore.Event) (map[string]interface{}, error) {
	eventData := make(map[string]interface{})
	if len(event.Data) > 0 {
		if err := json.Unmarshal(event.Data, &eventData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event data: %w", err)
		}
	}

	eventMetadata := make(map[string]interface{})
	if len(event.Metadata) > 0 {
		if err := json.Unmarshal(event.Metadata, &eventMetadata); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event metadata: %w", err)
		}
	}

	return map[string]interface{}{
		"id":            event.ID,
		"nid":           event.NID,
		"createdAt":     event.CreatedAt,
		"eventType":     event.EventType,
		"eventSource":   event.EventSource,
		"aggregateID":   event.AggregateID,
		"aggregateType": event.AggregateType,
		"version":       event.Version,
		"data":          eventData,
		"metadata":      eventMetadata,
	}, nil
}

// setupContextBoundBuiltins injects built-in functions bound to the given context.
// This must be called on each VM before each execution.
//
// Parameters:
//   - vm: Goja runtime instance
//   - ctx: Execution context
func (e *GojaExecutor) setupContextBoundBuiltins(ctx context.Context, vm *goja.Runtime) {
	// If we don't have services yet, skip builtins
	if e.jsEnv == nil {
		return
	}

	if err := e.jsEnv.SetupVM(ctx, vm, nil); err != nil {
		reportErr, _, end := e.tracker.Start(ctx, "setup", "jseval_builtins", "err", err.Error())
		defer end()
		reportErr(err)
	}
}

func (e *GojaExecutor) syncFunctionCache(ctx context.Context) error {
	// Track the sync operation
	reportErr, _, end := e.tracker.Start(ctx, "sync", "function_cache")
	defer end()

	// Step 1: Fetch all current functions from store
	allFunctions, err := e.functionService.ListAllFunctions(ctx)
	if err != nil {
		reportErr(fmt.Errorf("failed to list functions: %w", err))
		return err
	}

	// Build lookup map from DB
	dbFunctions := make(map[string]*functionstore.Function, len(allFunctions))
	for _, function := range allFunctions {
		dbFunctions[function.Name] = function
	}

	// Track which functions we've processed
	processed := make(map[string]bool)

	// Step 2: Validate cache against DB — remove stale/changed entries
	e.functionCache.Range(func(key, value interface{}) bool {
		name, ok := key.(string)
		if !ok {
			// Invalid key type — delete
			e.functionCache.Delete(name)
			_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "deleted_invalid_key", "function_name", name)
			reportChange(name, map[string]interface{}{"reason": "invalid_key_type"})
			return true
		}

		compiled, ok := value.(*compiledFunction)
		if !ok {
			// Invalid value type — delete
			e.functionCache.Delete(name)
			_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "deleted_invalid_value", "function_name", name)
			reportChange(name, map[string]interface{}{"reason": "invalid_cache_value"})
			return true
		}

		processed[name] = true

		dbFunc, exists := dbFunctions[name]
		if !exists {
			// Function deleted in DB — remove from cache
			e.functionCache.Delete(name)
			_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "deleted", "function_name", name)
			reportChange(name, map[string]interface{}{"reason": "not_in_db"})
			return true
		}

		// Compute current hash from DB script
		currentHash := e.computeCodeHash(dbFunc.Script)

		// TRUE DIFF: compare cached hash vs current DB hash
		if compiled.codeHash != currentHash {
			// Code changed — invalidate cache entry
			e.functionCache.Delete(name)
			_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "invalidated", "function_name", name)
			reportChange(name, map[string]interface{}{
				"old_hash": compiled.codeHash,
				"new_hash": currentHash,
				"reason":   "code_changed",
			})
		}

		return true
	})

	// Step 3: Load missing or invalidated functions into cache
	for name, f := range dbFunctions {
		if processed[name] {
			continue // already valid — skip
		}

		// Compile and cache
		reportErr, reportChange, end := e.tracker.Start(ctx, "sync", "function_cache", "action", "compile", "function_name", name)
		defer end()

		program, err := goja.Compile(name, f.Script, false)
		if err != nil {
			reportErr(fmt.Errorf("compile failed during sync: %w", err))
			continue
		}

		newHash := e.computeCodeHash(f.Script)
		compiled := &compiledFunction{
			program:  program,
			codeHash: newHash,
		}

		e.functionCache.Store(name, compiled)
		reportChange(name, map[string]interface{}{
			"code_hash": newHash,
			"reason":    "loaded_or_updated",
		})
	}

	return nil
}

func (e *GojaExecutor) SyncSingleFunction(ctx context.Context, functionName string) error {
	storedFunc, err := e.functionService.GetFunction(ctx, functionName)
	if err != nil {
		// Function doesn't exist — ensure cache is clean
		e.functionCache.Delete(functionName)
		_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "deleted", "function_name", functionName)
		reportChange(functionName, map[string]interface{}{"reason": "not_found_in_db"})
		return err
	}

	var currentHash = e.computeCodeHash(storedFunc.Script)

	// Check cache
	if cached, ok := e.functionCache.Load(functionName); ok {
		if compiled, ok := cached.(*compiledFunction); ok {
			// TRUE DIFF: compare cached hash with current
			if compiled.codeHash == currentHash {
				// No change — no-op
				return nil
			}
			// Changed — invalidate
			e.functionCache.Delete(functionName)
			_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "invalidated", "function_name", functionName)
			reportChange(functionName, map[string]interface{}{
				"old_hash": compiled.codeHash,
				"new_hash": currentHash,
				"reason":   "code_changed",
			})
		} else {
			// Corrupted cache entry
			e.functionCache.Delete(functionName)
			_, reportChange, _ := e.tracker.Start(ctx, "sync", "function_cache", "action", "deleted_invalid_value", "function_name", functionName)
			reportChange(functionName, map[string]interface{}{"reason": "corrupted_cache_entry"})
		}
	}

	// (Re)compile and cache
	reportErr, reportChange, end := e.tracker.Start(ctx, "sync", "function_cache", "action", "compile", "function_name", functionName)
	defer end()

	program, err := goja.Compile(functionName, storedFunc.Script, false)
	if err != nil {
		reportErr(fmt.Errorf("compile failed: %w", err))
		return err
	}

	compiled := &compiledFunction{
		program:  program,
		codeHash: currentHash,
	}

	e.functionCache.Store(functionName, compiled)
	reportChange(functionName, map[string]interface{}{
		"code_hash": currentHash,
		"reason":    "synced_from_db",
	})

	return nil
}

// computeCodeHash generates a deterministic hash for code comparison
func (e *GojaExecutor) computeCodeHash(code string) string {
	hash := sha1.Sum([]byte(code))
	return base64.StdEncoding.EncodeToString(hash[:])
}

// clearFunctionCache removes a function from the cache with tracking.
//
// Parameters:
//   - ctx: Execution context
//   - functionName: Name of the function to remove from cache
func (e *GojaExecutor) ClearFunctionCache(ctx context.Context, functionName string) {
	// Track cache clearance
	_, reportChange, end := e.tracker.Start(ctx, "clear", "function_cache",
		"function_name", functionName)
	defer end()

	e.functionCache.Delete(functionName)

	// Report successful clearance
	reportChange(functionName, nil)
}
