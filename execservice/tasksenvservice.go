package execservice

import (
	"context"

	"github.com/contenox/vibe/taskengine"
)

type TasksEnvService interface {
	Execute(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, inputType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error)
	taskengine.HookRegistry
}

type tasksEnvService struct {
	environmentExec taskengine.EnvExecutor
	hookRegistry    taskengine.HookRegistry
}

func NewTasksEnv(ctx context.Context, environmentExec taskengine.EnvExecutor, hookRegistry taskengine.HookRegistry) TasksEnvService {
	return &tasksEnvService{
		environmentExec: environmentExec,
		hookRegistry:    hookRegistry,
	}
}

func (s *tasksEnvService) Execute(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, inputType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	return s.environmentExec.ExecEnv(ctx, chain, input, inputType)
}

func (s *tasksEnvService) Supports(ctx context.Context) ([]string, error) {
	return s.hookRegistry.Supports(ctx)
}
