package libtracker

import "context"

// ActivityTracker defines a standard interface for instrumenting operations
// within an application. It acts as a hook mechanism to observe the lifecycle
// of an operation (start, potential error, potential state change, end)
// without tightly coupling the core logic to specific monitoring implementations.
//
// Implementations of this interface are typically used for:
//   - Recording metrics (latency, error rates, operation counts).
//   - Emitting structured logs at various lifecycle stages.
//   - Distributed tracing (creating and managing spans).
//   - Generating audit trails or activity streams, especially via `reportChange`.
//   - Tracking side effects or specific state changes.
//
// The core method is `Start`, which should be invoked at the beginning of
// the operation being tracked. It returns three functions (`reportErr`,
// `reportChange`, `end`) which *must* be used correctly to signal the
// outcome and completion of the operation.
//
// Correct Usage Pattern:
//
//  1. Call `Start` at the beginning of the operation.
//  2. Immediately `defer` the returned `end` function to ensure it's called
//     on function exit (signaling completion and allowing duration calculation).
//  3. Execute the core operation logic.
//  4. If the operation fails, call the returned `reportErr` function with the error.
//  5. If the operation succeeds *and* results in a reportable state change,
//     call the returned `reportChange` function with the relevant ID and optional data.
//
// Example:
//
//	// tracker is an instance of ActivityTracker
//	reportErr, reportChange, end := tracker.Start(ctx, "update", "user", userID, requestID)
//	defer end() // Ensures end() is called when the surrounding function returns
//
//	updatedUser, err := service.UpdateUser(ctx, userID, userData)
//	if err != nil {
//	    reportErr(err) // Report the error
//	    // return or handle error...
//	} else {
//	    // Optionally report the change, e.g., if auditing is needed
//	    reportChange(updatedUser.ID, updatedUser) // Report success and the resulting state
//	}
type ActivityTracker interface {
	// Start initiates the tracking of an operation.
	// It records the start time and context for the operation.
	//
	// Parameters:
	//   - ctx: The context for the operation, used for cancellation, deadlines,
	//          and carrying request-scoped values like trace IDs.
	//   - operation: A verb describing the action being performed (e.g., "create", "read", "process").
	//   - subject: A noun identifying the primary type of entity being acted upon (e.g., "user", "file", "order").
	//   - kvArgs: Optional key-value pairs or other metadata providing additional context
	//           at the start of the operation (e.g., relevant IDs, tags).
	//
	// Returns:
	//   - reportErr: A function to call *only* if the operation fails. Pass the error encountered.
	//   - reportChange: A function to call *only* if the operation succeeds *and* causes
	//                   a reportable state change. Pass the ID of the affected entity
	//                   and optional data about the change.
	//   - end: A function to call when the operation completes, regardless of success or failure.
	//          It signals the end of the tracked duration. Must be called exactly once.
	//          Typically called via `defer`.
	Start(
		ctx context.Context,
		operation string,
		subject string,
		kvArgs ...any,
	) (
		reportErr func(err error),
		reportChange func(id string, data any),
		// logEvent func(message string, level string, kvArgs ...any),
		end func(),
	)
}

// NoopTracker provides a no-operation implementation of the ActivityTracker interface.
// It adheres to the "Null Object Pattern".
//
// This implementation is useful when:
//   - Tracking needs to be disabled (e.g., in tests, specific environments, or via configuration)
//     without requiring conditional checks (`if tracker != nil`) in the calling code.
//   - Providing a safe default implementation when no specific tracker is configured.
//
// Using NoopTracker allows instrumentation calls (`Start`, `reportErr`, etc.) to remain
// in the code but incur minimal runtime overhead when tracking is inactive.
type NoopTracker struct{}

// Start returns three no-op functions that do nothing when called.
func (NoopTracker) Start(
	ctx context.Context,
	operation string,
	subject string,
	kvArgs ...any,
) (
	func(error),
	func(string, any),
	func(),
) {
	// Return functions that simply do nothing.
	return func(error) {}, func(string, any) {}, func() {}
}

var _ ActivityTracker = NoopTracker{}
