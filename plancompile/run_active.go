package plancompile

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/planstore"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
)

// PlanShowActive is implemented by planservice.Service for [RunActiveCompiled] without importing planservice (avoids cycles).
type PlanShowActive interface {
	Show(ctx context.Context) (string, error)
	Active(ctx context.Context) (*planstore.Plan, []*planstore.PlanStep, error)
}

// RunActiveCompiledResult is the outcome of compiling the active plan markdown and executing the compiled chain.
type RunActiveCompiledResult struct {
	Goal       string
	Steps      []string
	Chain      *taskengine.TaskChainDefinition
	Path       string
	Output     any
	OutputType taskengine.DataType
	State      []taskengine.CapturedStateUnit
}

// RunActiveCompiled loads markdown via planservice.Show, parses and compiles it with the executor chain,
// optionally persists the compiled JSON, then runs Execute with the plan goal as string input.
// precompiled, when non-empty, skips Compile (reuse planservice Next cache or explicit artifact).
// eventSink publishes plan_run_* events to the same bus/TaskEvent pipeline as task execution (optional; nil disables).
func RunActiveCompiled(
	ctx context.Context,
	planSvc PlanShowActive,
	chains taskchainservice.Service,
	exec execservice.TasksEnvService,
	executorChainID string,
	compiledChainID string,
	writePath string,
	precompiled *taskengine.TaskChainDefinition,
	eventSink taskengine.TaskEventSink,
) (*RunActiveCompiledResult, error) {
	if planSvc == nil || chains == nil || exec == nil {
		return nil, fmt.Errorf("plancompile: RunActiveCompiled: missing dependency")
	}
	executorChainID = strings.TrimSpace(executorChainID)
	compiledChainID = strings.TrimSpace(compiledChainID)
	if executorChainID == "" {
		return nil, fmt.Errorf("plancompile: executor_chain_id is required")
	}
	if compiledChainID == "" {
		return nil, fmt.Errorf("plancompile: chain_id is required")
	}

	taskengine.PublishPlanRunEvent(ctx, eventSink, taskengine.TaskEventPlanRunStarted, "")

	md, err := planSvc.Show(ctx)
	if err != nil {
		taskengine.PublishPlanRunFailed(ctx, eventSink, err)
		return nil, err
	}

	parsed, err := ParseMarkdown(md)
	if err != nil {
		taskengine.PublishPlanRunFailed(ctx, eventSink, err)
		return nil, fmt.Errorf("parse active plan markdown: %w", err)
	}

	var compiled *taskengine.TaskChainDefinition
	if precompiled != nil && len(precompiled.Tasks) > 0 {
		compiled = precompiled
	}

	if compiled == nil {
		execChain, err := chains.Get(ctx, executorChainID)
		if err != nil {
			taskengine.PublishPlanRunFailed(ctx, eventSink, err)
			return nil, fmt.Errorf("load executor chain: %w", err)
		}

		compiled, err = Compile(execChain, compiledChainID, parsed)
		if err != nil {
			taskengine.PublishPlanRunFailed(ctx, eventSink, err)
			return nil, fmt.Errorf("compile: %w", err)
		}
	}

	out := &RunActiveCompiledResult{
		Goal:  strings.TrimSpace(parsed.Goal),
		Steps: parsed.Steps,
		Chain: compiled,
	}

	if out.Goal == "" {
		plan, _, aerr := planSvc.Active(ctx)
		if aerr == nil && plan != nil {
			out.Goal = strings.TrimSpace(plan.Goal)
		}
	}

	wp := strings.TrimSpace(writePath)
	if wp != "" {
		if err := chains.CreateAtPath(ctx, wp, compiled); err != nil {
			taskengine.PublishPlanRunFailed(ctx, eventSink, err)
			return nil, fmt.Errorf("write compiled chain: %w", err)
		}
		out.Path = wp
	}

	taskengine.PublishPlanRunEvent(ctx, eventSink, taskengine.TaskEventPlanRunCompiled, compiled.ID)

	result, dt, state, err := exec.Execute(ctx, compiled, out.Goal, taskengine.DataTypeString)
	if err != nil {
		return nil, fmt.Errorf("execute compiled chain: %w", err)
	}

	out.Output = result
	out.OutputType = dt
	out.State = state
	return out, nil
}
