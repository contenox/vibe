package libtracker

import "context"

// ActivityTracker instruments an operation's lifecycle without coupling core
// logic to a monitoring backend. Start returns reportErr, reportChange and end;
// end must be called exactly once, the other two at most once each.
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

// NoopTracker is a null-object ActivityTracker.
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
