package stateservice

import (
	"context"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/statetype"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Get(ctx context.Context) ([]statetype.BackendRuntimeState, error) {
	// Start tracking the operation
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",  // operation type
		"state", // resource type
	)
	defer endFn()

	// Execute the actual service call
	stateMap, err := d.service.Get(ctx)

	if err != nil {
		reportErrFn(err)
	}

	return stateMap, err
}

// WithActivityTracker wraps a StateService with activity tracking
func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

// Ensure the decorator implements the Service interface
var _ Service = (*activityTrackerDecorator)(nil)
