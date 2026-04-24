// Package localhooks — plan_manager hook
// Exposes five tools to the chat LLM:
//
//   - create_plan(goal)      — generates a new step-by-step plan via the planner chain.
//   - run_next_step()        — executes the next pending step via the executor chain.
//   - get_plan_status()      — returns all steps with their current status and results.
//   - retry_step(ordinal)    — resets a failed/skipped step back to pending.
//   - skip_step(ordinal)     — marks a step as skipped so execution can continue.
//
// Tool responses flow back into the chat history as standard "tool" role messages,
// so the LLM naturally sees every step result without any manual history injection.
package localhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/contenox/contenox/runtime/execservice"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/planservice"
	"github.com/contenox/contenox/runtime/planstore"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/contenox/contenox/runtime/taskengine"
	"github.com/contenox/contenox/runtime/vfsservice"
	"github.com/getkin/kin-openapi/openapi3"
)

const planManagerHookName = "plan_manager"

// PlanManagerHook lets the chat LLM orchestrate plans via tool calls.
type PlanManagerHook struct {
	db              libdb.DBManager
	plannerChain    *taskengine.TaskChainDefinition
	executorChain   *taskengine.TaskChainDefinition
	summarizerChain *taskengine.TaskChainDefinition
	svc             planservice.Service
	contenoxDir     string
	workspaceID     string
}

func NewPlanManagerHook(
	db libdb.DBManager,
	plannerChain *taskengine.TaskChainDefinition,
	executorChain *taskengine.TaskChainDefinition,
	summarizerChain *taskengine.TaskChainDefinition,
	engine execservice.TasksEnvService,
	contenoxDir string,
	workspaceID string,
) taskengine.HookRepo {
	vfs := vfsservice.NewLocalFS(filepath.Join(contenoxDir, "plans"))
	return &PlanManagerHook{
		db:              db,
		plannerChain:    plannerChain,
		executorChain:   executorChain,
		summarizerChain: summarizerChain,
		contenoxDir:     contenoxDir,
		workspaceID:     workspaceID,
		svc:             planservice.New(db, engine, vfs, workspaceID),
	}
}

// Exec implements taskengine.HookRepo.
func (h *PlanManagerHook) Exec(
	ctx context.Context,
	_ time.Time,
	input any,
	_ bool,
	hook *taskengine.HookCall,
) (any, taskengine.DataType, error) {
	if hook == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager: hook call required")
	}

	switch hook.ToolName {
	case "create_plan":
		return h.createPlan(ctx, input, hook)
	case "run_next_step":
		return h.runNextStep(ctx)
	case "get_plan_status":
		return h.getPlanStatus(ctx)
	case "retry_step":
		return h.retryStep(ctx, input, hook)
	case "skip_step":
		return h.skipStep(ctx, input, hook)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager: unknown tool %q", hook.ToolName)
	}
}

// createPlan calls the planner chain and persists the new plan to SQLite.
func (h *PlanManagerHook) createPlan(ctx context.Context, input any, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	goal := extractGoal(input, hook)
	if goal == "" {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager create_plan: 'goal' argument is required")
	}

	plan, steps, _, err := h.svc.New(ctx, goal, h.plannerChain)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager create_plan: %w", err)
	}

	// Write the active-plan KV pointer so the TUI sidebar refreshes.
	kvStore := runtimetypes.New(h.db.WithoutTransaction())
	raw, _ := json.Marshal(plan.ID)
	err = kvStore.SetWorkspaceKV(ctx, h.workspaceID, "contenox.plan.active", json.RawMessage(raw))
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager create_plan: %w", err)
	}

	result := map[string]any{
		"plan_name": plan.Name,
		"goal":      plan.Goal,
		"steps":     len(steps),
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager create_plan: %w", err)
	}
	return string(out), taskengine.DataTypeString, nil
}

// runNextStep executes the next pending step via the executor chain.
func (h *PlanManagerHook) runNextStep(ctx context.Context) (any, taskengine.DataType, error) {
	// Peek at the next pending step for step metadata in the response.
	st := planstore.New(h.db.WithoutTransaction(), h.workspaceID)
	activePlan, err := st.GetActivePlan(ctx)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager run_next_step: %w", err)
	}
	steps, err := st.ListPlanSteps(ctx, activePlan.ID)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager run_next_step: %w", err)
	}
	var next *planstore.PlanStep
	for _, s := range steps {
		if s.Status == planstore.StepStatusPending {
			next = s
			break
		}
	}
	if next == nil {
		return `{"status":"done","message":"no pending steps remaining"}`, taskengine.DataTypeString, nil
	}

	result, _, execErr := h.svc.Next(ctx, planservice.Args{}, h.executorChain, h.summarizerChain)

	status := "completed"
	if execErr != nil {
		status = "failed"
		result = execErr.Error()
	}

	out, _ := json.Marshal(map[string]any{
		"ordinal":     next.Ordinal,
		"description": next.Description,
		"status":      status,
		"result":      result,
	})
	return string(out), taskengine.DataTypeString, nil
}

// getPlanStatus returns all steps of the active plan with their current state.
func (h *PlanManagerHook) getPlanStatus(ctx context.Context) (any, taskengine.DataType, error) {
	st := planstore.New(h.db.WithoutTransaction(), h.workspaceID)
	activePlan, err := st.GetActivePlan(ctx)
	if err != nil {
		return `{"status":"no_active_plan"}`, taskengine.DataTypeString, nil
	}
	steps, err := st.ListPlanSteps(ctx, activePlan.ID)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager get_plan_status: %w", err)
	}
	type stepView struct {
		Ordinal     int    `json:"ordinal"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Result      string `json:"result,omitempty"`
	}
	views := make([]stepView, len(steps))
	for i, s := range steps {
		views[i] = stepView{
			Ordinal:     s.Ordinal,
			Description: s.Description,
			Status:      string(s.Status),
			Result:      s.ExecutionResult,
		}
	}
	out, _ := json.Marshal(map[string]any{
		"plan_name": activePlan.Name,
		"goal":      activePlan.Goal,
		"status":    string(activePlan.Status),
		"steps":     views,
	})
	return string(out), taskengine.DataTypeString, nil
}

// retryStep resets a failed or skipped step back to pending.
func (h *PlanManagerHook) retryStep(ctx context.Context, input any, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	ordinal := extractOrdinal(input, hook)
	if ordinal <= 0 {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager retry_step: 'ordinal' argument is required and must be > 0")
	}
	_, err := h.svc.Retry(ctx, ordinal)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager retry_step: %w", err)
	}
	out, err := json.Marshal(map[string]any{"ordinal": ordinal, "status": "reset_to_pending"})
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager retry_step: %w", err)
	}
	return string(out), taskengine.DataTypeString, nil
}

// skipStep marks a step as skipped so execution can advance past it.
func (h *PlanManagerHook) skipStep(ctx context.Context, input any, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	ordinal := extractOrdinal(input, hook)
	if ordinal <= 0 {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager skip_step: 'ordinal' argument is required and must be > 0")
	}
	_, err := h.svc.Skip(ctx, ordinal)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager skip_step: %w", err)
	}
	out, err := json.Marshal(map[string]any{"ordinal": ordinal, "status": "skipped"})
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("plan_manager skip_step: %w", err)
	}
	return string(out), taskengine.DataTypeString, nil
}

// extractGoal reads the "goal" field from the tool call args or falls back to
// treating a plain string input as the goal.
func extractGoal(input any, hook *taskengine.HookCall) string {
	if hook != nil {
		if g, ok := hook.Args["goal"]; ok && g != "" {
			return g
		}
	}
	if m, ok := input.(map[string]any); ok {
		if g, ok := m["goal"].(string); ok {
			return g
		}
	}
	if s, ok := input.(string); ok {
		return s
	}
	return ""
}

// extractOrdinal reads the "ordinal" field from tool call args.
func extractOrdinal(input any, hook *taskengine.HookCall) int {
	if hook != nil {
		if raw, ok := hook.Args["ordinal"]; ok && raw != "" {
			var n int
			if _, err := fmt.Sscan(raw, &n); err == nil {
				return n
			}
		}
	}
	if m, ok := input.(map[string]any); ok {
		switch v := m["ordinal"].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

// Supports implements taskengine.HookRepo.
func (h *PlanManagerHook) Supports(_ context.Context) ([]string, error) {
	return []string{planManagerHookName}, nil
}

// GetSchemasForSupportedHooks implements taskengine.HookRepo.
func (h *PlanManagerHook) GetSchemasForSupportedHooks(_ context.Context) (map[string]*openapi3.T, error) {
	str := func(desc string) *openapi3.SchemaRef {
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: desc}}
	}
	integer := func(desc string) *openapi3.SchemaRef {
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeInteger}, Description: desc}}
	}
	obj := func(props openapi3.Schemas, required []string) *openapi3.SchemaRef {
		return &openapi3.SchemaRef{Value: &openapi3.Schema{
			Type:       &openapi3.Types{openapi3.TypeObject},
			Properties: props,
			Required:   required,
		}}
	}

	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info:    &openapi3.Info{Title: "Plan Manager Hook", Description: "LLM-driven plan orchestration tools", Version: "1.0.0"},
		Paths:   openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: openapi3.Schemas{
				// ── Requests ──────────────────────────────────────────────────
				"CreatePlanRequest": obj(openapi3.Schemas{
					"goal": str("High-level goal to plan (e.g. 'Analyze all Go files for TODOs')"),
				}, []string{"goal"}),
				"StepOrdinalRequest": obj(openapi3.Schemas{
					"ordinal": integer("1-based step number"),
				}, []string{"ordinal"}),
				// ── Responses ─────────────────────────────────────────────────
				"CreatePlanResponse": obj(openapi3.Schemas{
					"plan_name": str("Generated plan identifier"),
					"goal":      str("The goal that was planned"),
					"steps":     integer("Number of steps created"),
				}, nil),
				"RunNextStepResponse": obj(openapi3.Schemas{
					"ordinal":     integer("Step number that was executed (absent when done)"),
					"description": str("Step description (absent when done)"),
					"status":      str("completed | failed | done"),
					"result":      str("Execution output or error message"),
					"message":     str("Human-readable message (present when status=done)"),
				}, []string{"status"}),
				"PlanStatusResponse": obj(openapi3.Schemas{
					"plan_name": str("Active plan identifier"),
					"goal":      str("Plan goal"),
					"status":    str("active | completed | archived"),
					"steps": {Value: &openapi3.Schema{
						Type:        &openapi3.Types{openapi3.TypeArray},
						Description: "All steps in ordinal order",
						Items: obj(openapi3.Schemas{
							"ordinal":     integer("1-based step number"),
							"description": str("Step description"),
							"status":      str("pending | running | completed | failed | skipped"),
							"result":      str("Execution output (omitted when empty)"),
						}, []string{"ordinal", "description", "status"}),
					}},
				}, []string{"plan_name", "goal", "status", "steps"}),
				"StepOrdinalResponse": obj(openapi3.Schemas{
					"ordinal": integer("Step that was affected"),
					"status":  str("reset_to_pending | skipped"),
				}, []string{"ordinal", "status"}),
			},
		},
	}
	return map[string]*openapi3.T{planManagerHookName: schema}, nil
}

// GetToolsForHookByName implements taskengine.HookRepo.
func (h *PlanManagerHook) GetToolsForHookByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	if name != planManagerHookName {
		return nil, fmt.Errorf("plan_manager: unknown hook %q", name)
	}
	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "create_plan",
				Description: "Generate a new step-by-step execution plan for a given goal. Returns the plan name and step count. Call run_next_step repeatedly to execute each step.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"goal": map[string]any{
							"type":        "string",
							"description": "The high-level goal for the plan (e.g. 'Analyze all Go files for TODOs')",
						},
					},
					"required": []string{"goal"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "run_next_step",
				Description: "Execute the next pending step of the active plan. Returns ordinal, description, result, and status (completed/failed). Returns status=done when no steps remain.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "get_plan_status",
				Description: "Return the active plan and all its steps with their current status (pending/running/completed/failed/skipped) and result output. Use this to inspect progress or decide whether to retry or skip a failed step.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"required":   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "retry_step",
				Description: "Reset a failed or skipped step back to pending so it will be executed again by the next run_next_step call.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ordinal": map[string]any{
							"type":        "integer",
							"description": "1-based step number to retry",
						},
					},
					"required": []string{"ordinal"},
				},
			},
		},
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "skip_step",
				Description: "Mark a step as skipped so execution can advance past it without retrying. Use when a step is not relevant or cannot succeed given current conditions.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ordinal": map[string]any{
							"type":        "integer",
							"description": "1-based step number to skip",
						},
					},
					"required": []string{"ordinal"},
				},
			},
		},
	}, nil
}

var _ taskengine.HookRepo = (*PlanManagerHook)(nil)
