package toolsproviderservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
)

// activityTrackerDecorator implementation
type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

// NEW: Add tracking for ListLocalTools
func (d *activityTrackerDecorator) ListLocalTools(ctx context.Context) ([]LocalTools, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"list_local",
		"local_tools",
	)
	defer endFn()

	return d.service.ListLocalTools(ctx)
}

// GetSchemasForSupportedTools wraps the underlying service call with activity tracking.
func (d *activityTrackerDecorator) GetSchemasForSupportedTools(ctx context.Context) (map[string]*openapi3.T, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"get_schemas",
		"tools",
	)
	defer endFn()

	schemas, err := d.service.GetSchemasForSupportedTools(ctx)
	if err != nil {
		reportErrFn(err)
	}

	return schemas, err
}

func (d *activityTrackerDecorator) Create(ctx context.Context, tools *runtimetypes.RemoteTools) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"remote_tools",
		"name", tools.Name,
		"endpoint_url", tools.EndpointURL,
	)
	defer endFn()

	err := d.service.Create(ctx, tools)
	if err != nil {
		reportErrFn(err)
	} else {
		toolsData := struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			EndpointURL string `json:"endpointUrl"`
			TimeoutMs   int    `json:"timeoutMs"`
		}{
			ID:          tools.ID,
			Name:        tools.Name,
			EndpointURL: tools.EndpointURL,
			TimeoutMs:   tools.TimeoutMs,
		}
		reportChangeFn(tools.ID, toolsData)
	}

	return err
}

func (d *activityTrackerDecorator) Get(ctx context.Context, id string) (*runtimetypes.RemoteTools, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"get",
		"remote_tools",
		"id", id,
	)
	defer endFn()

	return d.service.Get(ctx, id)
}

func (d *activityTrackerDecorator) GetByName(ctx context.Context, name string) (*runtimetypes.RemoteTools, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"get_by_name",
		"remote_tools",
		"name", name,
	)
	defer endFn()

	return d.service.GetByName(ctx, name)
}

func (d *activityTrackerDecorator) Update(ctx context.Context, tools *runtimetypes.RemoteTools) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"remote_tools",
		"id", tools.ID,
		"name", tools.Name,
	)
	defer endFn()

	err := d.service.Update(ctx, tools)
	if err != nil {
		reportErrFn(err)
	} else {
		toolsData := struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			EndpointURL string `json:"endpointUrl"`
			TimeoutMs   int    `json:"timeoutMs"`
		}{
			ID:          tools.ID,
			Name:        tools.Name,
			EndpointURL: tools.EndpointURL,
			TimeoutMs:   tools.TimeoutMs,
		}
		reportChangeFn(tools.ID, toolsData)
	}

	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, id string) error {
	tools, err := d.service.Get(ctx, id)
	if err != nil {
		return err
	}

	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"remote_tools",
		"id", id,
		"name", tools.Name,
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

func (d *activityTrackerDecorator) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteTools, error) {
	_, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"remote_tools",
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
