package eventmappingservice

import (
	"context"

	"github.com/contenox/vibe/eventstore"
	"github.com/contenox/vibe/libtracker"
)

// activityTrackerDecorator implements Service with activity tracking
type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

// CreateMapping implements Service interface with activity tracking
func (d *activityTrackerDecorator) CreateMapping(ctx context.Context, config *eventstore.MappingConfig) error {
	if config == nil {
		return ErrInvalidMapping
	}

	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"mapping",
		"path", config.Path,
		"event_type", config.EventType,
		"aggregate_type", config.AggregateType,
		"version", config.Version,
	)
	defer endFn()

	err := d.service.CreateMapping(ctx, config)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(config.Path, map[string]interface{}{
			"event_type":         config.EventType,
			"event_source":       config.EventSource,
			"aggregate_type":     config.AggregateType,
			"aggregate_id_field": config.AggregateIDField,
			"version":            config.Version,
		})
	}

	return err
}

// GetMapping implements Service interface with activity tracking
func (d *activityTrackerDecorator) GetMapping(ctx context.Context, path string) (*eventstore.MappingConfig, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"get",
		"mapping",
		"path", path,
	)
	defer endFn()

	config, err := d.service.GetMapping(ctx, path)
	if err != nil {
		reportErrFn(err)
	}
	return config, err
}

// UpdateMapping implements Service interface with activity tracking
func (d *activityTrackerDecorator) UpdateMapping(ctx context.Context, config *eventstore.MappingConfig) error {
	if config == nil {
		return ErrInvalidMapping
	}

	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"mapping",
		"path", config.Path,
		"event_type", config.EventType,
		"aggregate_type", config.AggregateType,
		"version", config.Version,
	)
	defer endFn()

	err := d.service.UpdateMapping(ctx, config)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(config.Path, map[string]interface{}{
			"event_type":         config.EventType,
			"event_source":       config.EventSource,
			"aggregate_type":     config.AggregateType,
			"aggregate_id_field": config.AggregateIDField,
			"version":            config.Version,
		})
	}

	return err
}

// DeleteMapping implements Service interface with activity tracking
func (d *activityTrackerDecorator) DeleteMapping(ctx context.Context, path string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"mapping",
		"path", path,
	)
	defer endFn()

	err := d.service.DeleteMapping(ctx, path)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(path, map[string]interface{}{
			"operation": "deleted",
		})
	}

	return err
}

// ListMappings implements Service interface with activity tracking
func (d *activityTrackerDecorator) ListMappings(ctx context.Context) ([]*eventstore.MappingConfig, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"mappings",
	)
	defer endFn()

	mappings, err := d.service.ListMappings(ctx)
	if err != nil {
		reportErrFn(err)
	}
	return mappings, err
}

// WithActivityTracker decorates a Service with activity tracking
func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	if service == nil {
		panic("service cannot be nil")
	}
	if tracker == nil {
		panic("tracker cannot be nil")
	}
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

// Ensure the decorator implements the Service interface
var _ Service = (*activityTrackerDecorator)(nil)
