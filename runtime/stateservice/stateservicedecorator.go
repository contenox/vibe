package stateservice

import (
	"context"

	"github.com/contenox/contenox/runtime/internal/setupcheck"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/statetype"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Get(ctx context.Context) ([]statetype.BackendRuntimeState, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"state",
	)
	defer endFn()

	stateMap, err := d.service.Get(ctx)

	if err != nil {
		reportErrFn(err)
	}

	return stateMap, err
}

func (d *activityTrackerDecorator) SetupStatus(ctx context.Context) (setupcheck.Result, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"setup_status",
	)
	defer endFn()

	res, err := d.service.SetupStatus(ctx)
	if err != nil {
		reportErrFn(err)
	}
	return res, err
}

func (d *activityTrackerDecorator) SetCLIConfig(ctx context.Context, patch CLIConfigPatch) (CLIConfigSnapshot, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"update",
		"cli_config",
	)
	defer endFn()

	snap, err := d.service.SetCLIConfig(ctx, patch)
	if err != nil {
		reportErrFn(err)
	}
	return snap, err
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
