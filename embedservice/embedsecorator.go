package embedservice

import (
	"context"
	"fmt"

	"github.com/contenox/vibe/libtracker"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

// DefaultModelName implements Service.
func (d *activityTrackerDecorator) DefaultModelName(ctx context.Context) (string, error) {
	// Start tracking with relevant context
	reportErr, _, endFn := d.tracker.Start(
		ctx,
		"get_default_model",
		"embedding",
	)
	defer endFn()

	// Execute the operation
	modelName, err := d.service.DefaultModelName(ctx)
	if err != nil {
		// Report error with additional context
		reportErr(fmt.Errorf("failed to get default model: %w", err))
		return "", err
	}

	return modelName, nil
}
func (d *activityTrackerDecorator) Embed(ctx context.Context, text string) ([]float64, error) {
	// Start tracking with relevant context
	reportErr, _, endFn := d.tracker.Start(
		ctx,
		"embed",
		"embedding",
		"text_length", len(text),
	)
	defer endFn()

	// Execute the embedding operation
	vector, err := d.service.Embed(ctx, text)
	if err != nil {
		// Report error with additional context
		reportErr(fmt.Errorf("embedding failed: %w", err))
		return nil, err
	}

	return vector, nil
}

func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

var _ Service = (*activityTrackerDecorator)(nil)
