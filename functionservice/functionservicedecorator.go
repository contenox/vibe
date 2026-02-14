package functionservice

import (
	"context"
	"fmt"
	"time"

	"github.com/contenox/vibe/functionstore"
	"github.com/contenox/vibe/libtracker"
)

type activityTrackerDecorator struct {
	service Service
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerDecorator) CreateFunction(ctx context.Context, function *functionstore.Function) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"function",
		"name", function.Name,
		"scriptType", function.ScriptType,
	)
	defer endFn()

	err := d.service.CreateFunction(ctx, function)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(function.Name, map[string]interface{}{
			"name":       function.Name,
			"scriptType": function.ScriptType,
		})
	}

	return err
}

func (d *activityTrackerDecorator) GetFunction(ctx context.Context, name string) (*functionstore.Function, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"function",
		"functionName", name,
	)
	defer endFn()

	function, err := d.service.GetFunction(ctx, name)
	if err != nil {
		reportErrFn(err)
	}

	return function, err
}

func (d *activityTrackerDecorator) UpdateFunction(ctx context.Context, function *functionstore.Function) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"function",
		"functionName", function.Name,
	)
	defer endFn()

	err := d.service.UpdateFunction(ctx, function)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(function.Name, map[string]interface{}{
			"name":       function.Name,
			"scriptType": function.ScriptType,
		})
	}

	return err
}

func (d *activityTrackerDecorator) DeleteFunction(ctx context.Context, name string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"function",
		"functionName", name,
	)
	defer endFn()

	err := d.service.DeleteFunction(ctx, name)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(name, nil)
	}

	return err
}

func (d *activityTrackerDecorator) ListFunctions(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*functionstore.Function, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"functions",
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	functions, err := d.service.ListFunctions(ctx, createdAtCursor, limit)
	if err != nil {
		reportErrFn(err)
	}

	return functions, err
}

func (d *activityTrackerDecorator) ListAllFunctions(ctx context.Context) ([]*functionstore.Function, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list_all",
		"functions",
	)
	defer endFn()

	functions, err := d.service.ListAllFunctions(ctx)
	if err != nil {
		reportErrFn(err)
	}

	return functions, err
}

func (d *activityTrackerDecorator) CreateEventTrigger(ctx context.Context, trigger *functionstore.EventTrigger) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"create",
		"event_trigger",
		"name", trigger.Name,
		"eventType", trigger.ListenFor.Type,
		"function", trigger.Function,
	)
	defer endFn()

	err := d.service.CreateEventTrigger(ctx, trigger)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(trigger.Name, map[string]interface{}{
			"name":      trigger.Name,
			"eventType": trigger.ListenFor.Type,
			"function":  trigger.Function,
		})
	}

	return err
}

func (d *activityTrackerDecorator) GetEventTrigger(ctx context.Context, name string) (*functionstore.EventTrigger, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"read",
		"event_trigger",
		"triggerName", name,
	)
	defer endFn()

	trigger, err := d.service.GetEventTrigger(ctx, name)
	if err != nil {
		reportErrFn(err)
	}

	return trigger, err
}

func (d *activityTrackerDecorator) UpdateEventTrigger(ctx context.Context, trigger *functionstore.EventTrigger) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"update",
		"event_trigger",
		"triggerName", trigger.Name,
	)
	defer endFn()

	err := d.service.UpdateEventTrigger(ctx, trigger)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(trigger.Name, map[string]interface{}{
			"name":      trigger.Name,
			"eventType": trigger.ListenFor.Type,
			"function":  trigger.Function,
		})
	}

	return err
}

func (d *activityTrackerDecorator) DeleteEventTrigger(ctx context.Context, name string) error {
	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"delete",
		"event_trigger",
		"triggerName", name,
	)
	defer endFn()

	err := d.service.DeleteEventTrigger(ctx, name)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(name, nil)
	}

	return err
}

func (d *activityTrackerDecorator) ListEventTriggers(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*functionstore.EventTrigger, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list",
		"event_triggers",
		"cursor", fmt.Sprintf("%v", createdAtCursor),
		"limit", fmt.Sprintf("%d", limit),
	)
	defer endFn()

	triggers, err := d.service.ListEventTriggers(ctx, createdAtCursor, limit)
	if err != nil {
		reportErrFn(err)
	}

	return triggers, err
}

func (d *activityTrackerDecorator) ListAllEventTriggers(ctx context.Context) ([]*functionstore.EventTrigger, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list_all",
		"event_triggers",
	)
	defer endFn()

	triggers, err := d.service.ListAllEventTriggers(ctx)
	if err != nil {
		reportErrFn(err)
	}

	return triggers, err
}

func (d *activityTrackerDecorator) ListEventTriggersByEventType(ctx context.Context, eventType string) ([]*functionstore.EventTrigger, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list_by_event_type",
		"event_triggers",
		"eventType", eventType,
	)
	defer endFn()

	triggers, err := d.service.ListEventTriggersByEventType(ctx, eventType)
	if err != nil {
		reportErrFn(err)
	}

	return triggers, err
}

func (d *activityTrackerDecorator) ListEventTriggersByFunction(ctx context.Context, functionName string) ([]*functionstore.EventTrigger, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"list_by_function",
		"event_triggers",
		"functionName", functionName,
	)
	defer endFn()

	triggers, err := d.service.ListEventTriggersByFunction(ctx, functionName)
	if err != nil {
		reportErrFn(err)
	}

	return triggers, err
}

func WithActivityTracker(service Service, tracker libtracker.ActivityTracker) Service {
	return &activityTrackerDecorator{
		service: service,
		tracker: tracker,
	}
}

var _ Service = (*activityTrackerDecorator)(nil)
