package modelservice

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

func (d *activityTrackerDecorator) Append(ctx context.Context, model *runtimetypes.Model) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"model",
		"name", model.Model,
	)
	defer endFn()

	err := d.service.Append(ctx, model)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(model.Model, map[string]interface{}{
			"name": model.Model,
		})
	}

	return err
}

func (d *activityTrackerDecorator) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Model, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"models",
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	models, err := d.service.List(ctx, createdAtCursor, limit)
	if err != nil {
		reportErrFn(err)
	}

	return models, err
}

func (d *activityTrackerDecorator) Update(ctx context.Context, data *runtimetypes.Model) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"model",
		"name", data.Model,
		"id", data.ID,
	)
	defer endFn()

	err := d.service.Update(ctx, data)
	if err != nil {
		reportErrFn(err)
	} else {
		changes := map[string]any{
			"model":         data.Model,
			"contextLength": data.ContextLength,
			"canChat":       data.CanChat,
			"canEmbed":      data.CanEmbed,
			"canPrompt":     data.CanPrompt,
			"canStream":     data.CanStream,
		}
		reportChangeFn(data.ID, changes)
	}

	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, modelName string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"model",
		"name", modelName,
	)
	defer endFn()

	err := d.service.Delete(ctx, modelName)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(modelName, "deleted")
	}

	return err
}

func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}
