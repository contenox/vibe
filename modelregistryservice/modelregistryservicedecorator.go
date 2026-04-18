package modelregistryservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtimetypes"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Create(ctx context.Context, e *runtimetypes.ModelRegistryEntry) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx, "create", "model-registry-entry",
		"name", e.Name, "sourceUrl", e.SourceURL,
	)
	defer endFn()
	err := d.service.Create(ctx, e)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(e.ID, map[string]interface{}{"name": e.Name, "sourceUrl": e.SourceURL})
	}
	return err
}

func (d *activityTrackerDecorator) Get(ctx context.Context, id string) (*runtimetypes.ModelRegistryEntry, error) {
	reportErrFn, _, endFn := d.tracker.Start(ctx, "read", "model-registry-entry", "id", id)
	defer endFn()
	e, err := d.service.Get(ctx, id)
	if err != nil {
		reportErrFn(err)
	}
	return e, err
}

func (d *activityTrackerDecorator) GetByName(ctx context.Context, name string) (*runtimetypes.ModelRegistryEntry, error) {
	reportErrFn, _, endFn := d.tracker.Start(ctx, "read", "model-registry-entry", "name", name)
	defer endFn()
	e, err := d.service.GetByName(ctx, name)
	if err != nil {
		reportErrFn(err)
	}
	return e, err
}

func (d *activityTrackerDecorator) Update(ctx context.Context, e *runtimetypes.ModelRegistryEntry) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx, "update", "model-registry-entry",
		"id", e.ID, "name", e.Name,
	)
	defer endFn()
	err := d.service.Update(ctx, e)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(e.ID, map[string]interface{}{"name": e.Name, "sourceUrl": e.SourceURL})
	}
	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, id string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(ctx, "delete", "model-registry-entry", "id", id)
	defer endFn()
	err := d.service.Delete(ctx, id)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(id, nil)
	}
	return err
}

func (d *activityTrackerDecorator) List(ctx context.Context, cursor *time.Time, limit int) ([]*runtimetypes.ModelRegistryEntry, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx, "list", "model-registry-entries",
		"cursor", fmt.Sprintf("%v", cursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()
	entries, err := d.service.List(ctx, cursor, limit)
	if err != nil {
		reportErrFn(err)
	}
	return entries, err
}

func WithActivityTracker(svc Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{service: svc, tracker: tracker}
}

var _ Service = (*activityTrackerDecorator)(nil)
