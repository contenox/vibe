package backendservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/runtimetypes"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Create(ctx context.Context, backend *runtimetypes.Backend) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"backend",
		"name", backend.Name,
		"type", backend.Type,
		"baseURL", backend.BaseURL,
	)
	defer endFn()

	err := d.service.Create(ctx, backend)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(backend.ID, map[string]interface{}{
			"name":    backend.Name,
			"type":    backend.Type,
			"baseURL": backend.BaseURL,
		})
	}

	return err
}

func (d *activityTrackerDecorator) Get(ctx context.Context, id string) (*runtimetypes.Backend, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"backend",
		"backendID", id,
	)
	defer endFn()

	backend, err := d.service.Get(ctx, id)
	if err != nil {
		reportErrFn(err)
	}

	return backend, err
}

func (d *activityTrackerDecorator) Update(ctx context.Context, backend *runtimetypes.Backend) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"backend",
		"backendID", backend.ID,
		"name", backend.Name,
	)
	defer endFn()

	err := d.service.Update(ctx, backend)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(backend.ID, map[string]interface{}{
			"name":    backend.Name,
			"type":    backend.Type,
			"baseURL": backend.BaseURL,
		})
	}

	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, id string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"backend",
		"backendID", id,
	)
	defer endFn()

	err := d.service.Delete(ctx, id)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(id, nil)
	}

	return err
}

func (d *activityTrackerDecorator) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Backend, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"backends",
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	backends, err := d.service.List(ctx, createdAtCursor, limit)
	if err != nil {
		reportErrFn(err)
	}

	return backends, err
}

func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

var _ Service = (*activityTrackerDecorator)(nil)
