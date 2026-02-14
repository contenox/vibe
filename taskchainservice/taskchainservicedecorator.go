package taskchainservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Create(ctx context.Context, chain *taskengine.TaskChainDefinition) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"taskchain",
		"id", chain.ID,
	)
	defer endFn()

	err := d.service.Create(ctx, chain)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(chain.ID, map[string]interface{}{
			"id":          chain.ID,
			"description": chain.Description,
			"taskCount":   len(chain.Tasks),
		})
	}

	return err
}

func (d *activityTrackerDecorator) Get(ctx context.Context, id string) (*taskengine.TaskChainDefinition, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"get",
		"taskchain",
		"id", id,
	)
	defer endFn()

	chain, err := d.service.Get(ctx, id)
	if err != nil {
		reportErrFn(err)
	}

	return chain, err
}

func (d *activityTrackerDecorator) Update(ctx context.Context, chain *taskengine.TaskChainDefinition) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"taskchain",
		"id", chain.ID,
	)
	defer endFn()

	err := d.service.Update(ctx, chain)
	if err != nil {
		reportErrFn(err)
	} else {
		// Only report metadata to avoid logging sensitive task details
		changes := map[string]interface{}{
			"description": chain.Description,
			"debug":       chain.Debug,
			"tokenLimit":  chain.TokenLimit,
			"taskCount":   len(chain.Tasks),
		}
		reportChangeFn(chain.ID, changes)
	}

	return err
}

func (d *activityTrackerDecorator) Delete(ctx context.Context, id string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"taskchain",
		"id", id,
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

func (d *activityTrackerDecorator) List(ctx context.Context, cursor *time.Time, limit int) ([]*taskengine.TaskChainDefinition, error) {
	cursorStr := "nil"
	if cursor != nil {
		cursorStr = cursor.Format(time.RFC3339)
	}

	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"taskchains",
		"cursor", cursorStr,
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	chains, err := d.service.List(ctx, cursor, limit)
	if err != nil {
		reportErrFn(err)
	}

	return chains, err
}

// WithActivityTracker wraps a task chain service with activity tracking capabilities
func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}
