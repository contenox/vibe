package libtracker

import "context"

// ActivityTracker instruments an operation's lifecycle (start, optional
// error, optional state change, end) without coupling core logic to a
// specific monitoring backend. Start returns reportErr, reportChange, and
// end: end must be called exactly once, typically via defer; reportErr and
// reportChange are called at most once each, depending on outcome.
type ActivityTracker interface {
	Start(
		ctx context.Context,
		operation string,
		subject string,
		kvArgs ...any,
	) (
		reportErr func(err error),
		reportChange func(id string, data any),
		end func(),
	)
}

// NoopTracker is a null-object ActivityTracker for tests and environments
// where tracking is disabled.
type NoopTracker struct{}

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
	return func(error) {}, func(string, any) {}, func() {}
}

var _ ActivityTracker = NoopTracker{}
