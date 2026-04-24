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

// FailureClass tags WHY a step failed, so callers (e.g. plan next --auto) can
// decide whether to auto-replan, retry transparently, or surface to the user.
//
// FailureClassEmpty is the default for unknown / legacy rows. FailureClassLogic
// is the catch-all for non-transient model/tool failures (e.g. the model said
// "I can't do X"). FailureClassCapacity means the chain hit a context-length
// or token-budget wall; the plan can usefully be replanned into smaller steps.
// FailureClassTransient is reserved for retries-exhausted transient HTTP errors
// (rate-limit / 5xx / timeout).
type FailureClass string

const (
	FailureClassEmpty     FailureClass = ""
	FailureClassLogic     FailureClass = "logic"
	FailureClassCapacity  FailureClass = "capacity"
	FailureClassTransient FailureClass = "transient"
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
	// RepoContextJSON is a [RepoContext] document produced by the explorer chain
	// and rendered into every step's seed prompt as {{var:repo_context}}.
	// Empty string means the plan has no explored context (current default).
	RepoContextJSON string    `json:"repo_context_json,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
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

	// Summary is a JSON document (schema: outcome/summary/artifacts/handover_for_next/caveats)
	// produced by the summarizer chain and persisted by the plan_summary persist tools.
	// Empty string means NULL (legacy row or summarizer fell through to fallback).
	Summary string `json:"summary,omitempty"`
	// ChatHistoryJSON is the raw taskengine.ChatHistory from the executor subgraph,
	// preserved for debugging and for Retry prior-attempt context.
	ChatHistoryJSON string `json:"chat_history_json,omitempty"`
	// SummaryError is the raw summarizer output + parse/validation error when
	// both the first summarizer attempt and its repair attempt failed validation.
	// Populated only on fallback-path persistence.
	SummaryError string `json:"summary_error,omitempty"`
	// LastFailureSummary is the Summary (or ExecutionResult fallback) from the
	// previous failed attempt of this step, moved here by Retry. Lets the next
	// attempt's summarizer see why the prior try failed.
	LastFailureSummary string `json:"last_failure_summary,omitempty"`
	// FailureClass classifies the most recent failure so callers can pick a
	// recovery strategy (retry / replan / surface). Empty when the step is not
	// failed or when the failure was not classified.
	FailureClass FailureClass `json:"failure_class,omitempty"`
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
	// UpdatePlanRepoContext persists the RepoContext JSON for the plan.
	// Pass empty string to clear it. Compile cache is NOT invalidated by this
	// call; the seed-prompt template var is read at run time.
	UpdatePlanRepoContext(ctx context.Context, planID string, repoContextJSON string) error

	// Step operations
	CreatePlanSteps(ctx context.Context, steps ...*PlanStep) error
	ListPlanSteps(ctx context.Context, planID string) ([]*PlanStep, error)
	UpdatePlanStepStatus(ctx context.Context, stepID string, status StepStatus, result string) error
	DeletePendingPlanSteps(ctx context.Context, planID string) error
	// ClaimNextPendingStep atomically marks the next pending step as running
	// and returns it. Returns ErrNotFound when no pending step exists.
	ClaimNextPendingStep(ctx context.Context, planID string) (*PlanStep, error)

	// UpdatePlanStepSummary persists a validated summary JSON + raw chat history for the step.
	// Called by the plan_summary persist tools on successful validation.
	UpdatePlanStepSummary(ctx context.Context, stepID string, summaryJSON, chatHistoryJSON string) error
	// UpdatePlanStepSummaryFailure persists the raw summarizer output + parse/validation error
	// and updates ExecutionResult to a fallback string so the next step still has context.
	// Called by the plan_summary fallback tools when validation failed twice.
	UpdatePlanStepSummaryFailure(ctx context.Context, stepID string, rawOutput, errMsg, fallbackExecResult string) error
	// MoveSummaryToLastFailure atomically copies the current Summary (or ExecutionResult
	// fallback) into LastFailureSummary and clears Summary/ChatHistoryJSON/SummaryError.
	// Called by Retry so the re-run's summarizer can see the prior failed attempt's context.
	MoveSummaryToLastFailure(ctx context.Context, stepID string) error
	// SetPlanStepFailureClass records the [FailureClass] of the most recent
	// failure on a step. Pass [FailureClassEmpty] to clear it (e.g. on retry).
	SetPlanStepFailureClass(ctx context.Context, stepID string, class FailureClass) error

	// Bulk operations for efficiency
	// DeleteFinishedPlans removes all completed/archived plans; returns count.
	DeleteFinishedPlans(ctx context.Context) (int, error)
	// ArchiveActivePlans sets all active plans to archived status.
	ArchiveActivePlans(ctx context.Context) error
}
