package execservice

import (
	"context"
	"fmt"

	"github.com/contenox/vibe/libtracker"
	"github.com/contenox/vibe/taskengine"
)

type activityTrackerTaskEnvDecorator struct {
	service TasksEnvService
	tracker libtracker.ActivityTracker
}

func (d *activityTrackerTaskEnvDecorator) Execute(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, inputType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	// Extract useful metadata from the chain
	chainID := chain.ID

	reportErrFn, reportChangeFn, endFn := d.tracker.Start(
		ctx,
		"execute",
		"task-chain",
		"chainID", chainID,
		"inputLength", len(fmt.Sprintf("%v", input)),
		"inputType", inputType.String(),
	)
	defer endFn()

	result, outputType, stacktrace, err := d.service.Execute(ctx, chain, input, inputType)
	if err != nil {
		reportErrFn(err)
	} else {
		reportChangeFn(chainID, map[string]any{
			"input":      input,
			"result":     result,
			"chainID":    chainID,
			"stacktrace": stacktrace,
			"outputType": outputType.String(),
		})
	}

	return result, outputType, stacktrace, err
}

func (d *activityTrackerTaskEnvDecorator) Supports(ctx context.Context) ([]string, error) {
	return d.service.Supports(ctx)
}

func EnvWithActivityTracker(service TasksEnvService, tracker libtracker.ActivityTracker) TasksEnvService {
	return &activityTrackerTaskEnvDecorator{
		service: service,
		tracker: tracker,
	}
}
