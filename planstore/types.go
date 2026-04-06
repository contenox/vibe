package planstore

import (
	"context"
	"time"
)

// PlanStatus represents the current state of a plan.
type PlanStatus string

const (
	PlanStatusActive    PlanStatus = "active"
	PlanStatusCompleted PlanStatus = "completed"
	PlanStatusArchived  PlanStatus = "archived"
)

// StepStatus represents the current state of a plan step.
type StepStatus string

const (
	StepStatusPending   StepStatus = "pending"
	StepStatusRunning   StepStatus = "running"
	StepStatusCompleted StepStatus = "completed"
	StepStatusFailed    StepStatus = "failed"
	StepStatusSkipped   StepStatus = "skipped"
)

// Plan maps to the plans table.
type Plan struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Goal      string     `json:"goal"`
	Status    PlanStatus `json:"status"`
	SessionID string     `json:"session_id"`
	// CompiledChainJSON is the full [plancompile.Compile] output JSON for incremental Next (optional).
	CompiledChainJSON string `json:"compiled_chain_json,omitempty"`
	// CompiledChainID is the TaskChainDefinition.ID used when compiling (for invalidation).
	CompiledChainID string `json:"compiled_chain_id,omitempty"`
	// CompileExecutorChainID is the executor chain ID used when compiling (for invalidation).
	CompileExecutorChainID string `json:"compile_executor_chain_id,omitempty"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

// PlanStep maps to the plan_steps table.
type PlanStep struct {
	ID              string     `json:"id"`
	PlanID          string     `json:"plan_id"`
	Ordinal         int        `json:"ordinal"`
	Description     string     `json:"description"`
	Status          StepStatus `json:"status"`
	ExecutionResult string     `json:"execution_result"`
	ExecutedAt      time.Time  `json:"executed_at"` // Zero time if not executed
}

// Store defines the data access interface for plans and steps.
type Store interface {
	// Plan operations
	CreatePlan(ctx context.Context, plan *Plan) error
	GetPlanByID(ctx context.Context, id string) (*Plan, error)
	GetPlanByName(ctx context.Context, name string) (*Plan, error)
	// GetActivePlan returns the most recently updated active plan, or ErrNotFound.
	GetActivePlan(ctx context.Context) (*Plan, error)
	ListPlans(ctx context.Context) ([]*Plan, error)
	DeletePlan(ctx context.Context, id string) error
	UpdatePlanStatus(ctx context.Context, planID string, status PlanStatus) error
	// UpdatePlanCompiledChain persists compile output for the plan (empty json clears compile cache).
	UpdatePlanCompiledChain(ctx context.Context, planID string, compiledChainJSON, compiledChainID, executorChainID string) error

	// Step operations
	CreatePlanSteps(ctx context.Context, steps ...*PlanStep) error
	ListPlanSteps(ctx context.Context, planID string) ([]*PlanStep, error)
	UpdatePlanStepStatus(ctx context.Context, stepID string, status StepStatus, result string) error
	DeletePendingPlanSteps(ctx context.Context, planID string) error
	// ClaimNextPendingStep atomically marks the next pending step as running
	// and returns it. Returns ErrNotFound when no pending step exists.
	ClaimNextPendingStep(ctx context.Context, planID string) (*PlanStep, error)

	// Bulk operations for efficiency
	// DeleteFinishedPlans removes all completed/archived plans; returns count.
	DeleteFinishedPlans(ctx context.Context) (int, error)
	// ArchiveActivePlans sets all active plans to archived status.
	ArchiveActivePlans(ctx context.Context) error
}
