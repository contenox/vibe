package hookproviderservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
)

// activityTrackerDecorator implementation
type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

// NEW: Add tracking for ListLocalHooks
func (d *activityTrackerDecorator) ListLocalHooks(ctx context.Context) ([]LocalHook, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"list_local",
		"local_hooks",
	)
	defer endFn()

	return d.service.ListLocalHooks(ctx)
}

// GetSchemasForSupportedHooks wraps the underlying service call with activity tracking.
func (d *activityTrackerDecorator) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"get_schemas",
		"hooks",
	)
	defer endFn()

	schemas, err := d.service.GetSchemasForSupportedHooks(ctx)
	if err != nil {
		reportErrFn(err)
	}

	return schemas, err
}

func (d *activityTrackerDecorator) Create(ctx context.Context, hook *runtimetypes.RemoteHook) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"remote_hook",
		"name", hook.Name,
		"endpoint_url", hook.EndpointURL,
	)
	defer endFn()

	err := d.service.Create(ctx, hook)
	if err != nil {
		reportErrFn(err)
	} else {
		hookData := struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			EndpointURL string `json:"endpointUrl"`
			TimeoutMs   int    `json:"timeoutMs"`
		}{
			ID:          hook.ID,
			Name:        hook.Name,
			EndpointURL: hook.EndpointURL,
			TimeoutMs:   hook.TimeoutMs,
		}
		reportChangeFn(hook.ID, hookData)
	}

	return err
}

func (d *activityTrackerDecorator) Get(ctx context.Context, id string) (*runtimetypes.RemoteHook, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"get",
		"remote_hook",
		"id", id,
	)
	defer endFn()

	return d.service.Get(ctx, id)
}

func (d *activityTrackerDecorator) GetByName(ctx context.Context, name string) (*runtimetypes.RemoteHook, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"get_by_name",
		"remote_hook",
		"name", name,
	)
	defer endFn()

	return d.service.GetByName(ctx, name)
}

func (d *activityTrackerDecorator) Update(ctx context.Context, hook *runtimetypes.RemoteHook) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"remote_hook",
		"id", hook.ID,
		"name", hook.Name,
	)
	defer endFn()

	err := d.service.Update(ctx, hook)
	if err != nil {
		reportErrFn(err)
	} else {
		hookData := struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			EndpointURL string `json:"endpointUrl"`
			TimeoutMs   int    `json:"timeoutMs"`
		}{
			ID:          hook.ID,
			Name:        hook.Name,
			EndpointURL: hook.EndpointURL,
			TimeoutMs:   hook.TimeoutMs,
		}
		reportChangeFn(hook.ID, hookData)
	}

	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, id string) error {
	hook, err := d.service.Get(ctx, id)
	if err != nil {
		return err
	}

	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"remote_hook",
		"id", id,
		"name", hook.Name,
	)
	defer endFn()

	err = d.service.Delete(ctx, id)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(id, nil)
	}

	return err
}

func (d *activityTrackerDecorator) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteHook, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"remote_hooks",
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	return d.service.List(ctx, createdAtCursor, limit)
}

// WithActivityTracker wraps a Service with activity tracking functionality.
func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}
