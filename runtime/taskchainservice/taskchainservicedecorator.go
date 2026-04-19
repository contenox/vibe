package taskchainservice

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/taskengine"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) Get(ctx context.Context, ref string) (*taskengine.TaskChainDefinition, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"get",
		"taskchain",
		"ref", ref,
	)
	defer endFn()

	chain, err := d.service.Get(ctx, ref)
	if err != nil {
		reportErrFn(err)
	}

	return chain, err
}

func (d *activityTrackerDecorator) List(ctx context.Context) ([]string, error) {
	reportErrFn, _, endFn := d.tracker.Start(ctx, "list", "taskchain")
	defer endFn()

	paths, err := d.service.List(ctx)
	if err != nil {
		reportErrFn(err)
	}
	return paths, err
}

func (d *activityTrackerDecorator) CreateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"taskchain",
		"path", path,
		"id", chain.ID,
	)
	defer endFn()

	err := d.service.CreateAtPath(ctx, path, chain)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(chain.ID, map[string]interface{}{
			"path":        path,
			"id":          chain.ID,
			"description": chain.Description,
			"taskCount":   len(chain.Tasks),
		})
	}

	return err
}

func (d *activityTrackerDecorator) UpdateAtPath(ctx context.Context, path string, chain *taskengine.TaskChainDefinition) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"taskchain",
		"path", path,
		"id", chain.ID,
	)
	defer endFn()

	err := d.service.UpdateAtPath(ctx, path, chain)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(chain.ID, map[string]interface{}{
			"path":        path,
			"description": chain.Description,
			"debug":       chain.Debug,
			"tokenLimit":  chain.TokenLimit,
			"taskCount":   len(chain.Tasks),
		})
	}

	return err
}

func (d *activityTrackerDecorator) DeleteByPath(ctx context.Context, path string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"taskchain",
		"path", path,
	)
	defer endFn()

	err := d.service.DeleteByPath(ctx, path)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(path, nil)
	}

	return err
}

// WithActivityTracker wraps a task chain service with activity tracking capabilities.
func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	if tracker == nil {
		return service
	}
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

var _ Service = (*activityTrackerDecorator)(nil)

// Compile-time guard: activityTrackerDecorator must implement the full interface.
func _noopChainDecoratorInterface() {
	var _ Service = (*activityTrackerDecorator)(nil)
}

func _unusedChainFmt() {
	_ = fmt.Sprint()
}
