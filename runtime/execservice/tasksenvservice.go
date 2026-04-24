package execservice

import (
	"context"

	"github.com/contenox/contenox/runtime/taskengine"
)

type TasksEnvService interface {
	Execute(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, inputType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error)
	taskengine.ToolsRegistry
}

type tasksEnvService struct {
	environmentExec taskengine.EnvExecutor
	toolsRegistry    taskengine.ToolsRegistry
}

func NewTasksEnv(ctx context.Context, environmentExec taskengine.EnvExecutor, toolsRegistry taskengine.ToolsRegistry) TasksEnvService {
	return &tasksEnvService{
		environmentExec: environmentExec,
		toolsRegistry:    toolsRegistry,
	}
}

func (s *tasksEnvService) Execute(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, inputType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error) {
	return s.environmentExec.ExecEnv(ctx, chain, input, inputType)
}

func (s *tasksEnvService) Supports(ctx context.Context) ([]string, error) {
	return s.toolsRegistry.Supports(ctx)
}
