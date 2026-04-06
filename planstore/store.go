package planstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/libdbexec"
)

var ErrNotFound = errors.New("plan not found")

type store struct {
	Exec libdbexec.Exec
}

// New creates a new plan store instance.
func New(exec libdbexec.Exec) Store {
	return &store{Exec: exec}
}

// CreatePlan creates a new plan.
func (s *store) CreatePlan(ctx context.Context, plan *Plan) error {
	now := time.Now().UTC()
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = now
	}
	if plan.UpdatedAt.IsZero() {
		plan.UpdatedAt = now
	}
	if plan.Status == "" {
		plan.Status = PlanStatusActive
	}

	sessionID := sql.NullString{String: plan.SessionID, Valid: plan.SessionID != ""}
	ccJSON := sql.NullString{String: plan.CompiledChainJSON, Valid: plan.CompiledChainJSON != ""}
	ccID := sql.NullString{String: plan.CompiledChainID, Valid: plan.CompiledChainID != ""}
	exID := sql.NullString{String: plan.CompileExecutorChainID, Valid: plan.CompileExecutorChainID != ""}

	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO plans (id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		plan.ID,
		plan.Name,
		plan.Goal,
		string(plan.Status),
		sessionID,
		ccJSON,
		ccID,
		exID,
		plan.CreatedAt,
		plan.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create plan: %w", err)
	}
	return nil
}

func (s *store) GetPlanByID(ctx context.Context, id string) (*Plan, error) {
	return s.getPlanByCondition(ctx, "id = $1", id)
}

func (s *store) GetPlanByName(ctx context.Context, name string) (*Plan, error) {
	return s.getPlanByCondition(ctx, "name = $1", name)
}

func (s *store) getPlanByCondition(ctx context.Context, condition string, arg any) (*Plan, error) {
	var p Plan
	var sessionID sql.NullString
	var ccJSON, ccID, exID sql.NullString
	var status string

	query := fmt.Sprintf(`
		SELECT id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, created_at, updated_at
		FROM plans
		WHERE %s`, condition)

	err := s.Exec.QueryRowContext(ctx, query, arg).Scan(
		&p.ID,
		&p.Name,
		&p.Goal,
		&status,
		&sessionID,
		&ccJSON,
		&ccID,
		&exID,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get plan: %w", err)
	}
	p.Status = PlanStatus(status)
	if sessionID.Valid {
		p.SessionID = sessionID.String
	}
	if ccJSON.Valid {
		p.CompiledChainJSON = ccJSON.String
	}
	if ccID.Valid {
		p.CompiledChainID = ccID.String
	}
	if exID.Valid {
		p.CompileExecutorChainID = exID.String
	}
	return &p, nil
}

// GetActivePlan returns the most recently updated active plan.
func (s *store) GetActivePlan(ctx context.Context) (*Plan, error) {
	var p Plan
	var sessionID sql.NullString
	var status string
	var ccJSON, ccID, exID sql.NullString
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, created_at, updated_at
		FROM plans
		WHERE status = 'active'
		ORDER BY updated_at DESC
		LIMIT 1`,
	).Scan(&p.ID, &p.Name, &p.Goal, &status, &sessionID, &ccJSON, &ccID, &exID, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get active plan: %w", err)
	}
	p.Status = PlanStatus(status)
	if sessionID.Valid {
		p.SessionID = sessionID.String
	}
	if ccJSON.Valid {
		p.CompiledChainJSON = ccJSON.String
	}
	if ccID.Valid {
		p.CompiledChainID = ccID.String
	}
	if exID.Valid {
		p.CompileExecutorChainID = exID.String
	}
	return &p, nil
}

// ClaimNextPendingStep atomically transitions the next pending step to running
// and returns it. Returns ErrNotFound when no pending step exists.
// FOR UPDATE SKIP LOCKED ensures two concurrent callers cannot claim the same step.
func (s *store) ClaimNextPendingStep(ctx context.Context, planID string) (*PlanStep, error) {
	var step PlanStep
	var status string
	var execAt sql.NullTime
	query := `
		UPDATE plan_steps
		SET status = 'running'
		WHERE id = (
			SELECT id FROM plan_steps
			WHERE plan_id = $1 AND status = 'pending'
			ORDER BY ordinal ASC
			LIMIT 1
			{{.Locking}}
		)
		AND status = 'pending'
		RETURNING id, plan_id, ordinal, description, status, execution_result, executed_at`

	locking := ""
	if s.Exec.DriverName() == "postgres" {
		locking = "FOR UPDATE SKIP LOCKED"
	}
	query = strings.Replace(query, "{{.Locking}}", locking, 1)

	err := s.Exec.QueryRowContext(ctx, query, planID).Scan(&step.ID, &step.PlanID, &step.Ordinal, &step.Description, &status, &step.ExecutionResult, &execAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to claim next pending step: %w", err)
	}
	step.Status = StepStatus(status)
	if execAt.Valid {
		step.ExecutedAt = execAt.Time
	}
	return &step, nil
}

func (s *store) ListPlans(ctx context.Context) ([]*Plan, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, created_at, updated_at
		FROM plans
		ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query plans: %w", err)
	}
	defer rows.Close()

	var plans []*Plan
	for rows.Next() {
		var p Plan
		var sessionID sql.NullString
		var ccJSON, ccID, exID sql.NullString
		var status string
		if err := rows.Scan(&p.ID, &p.Name, &p.Goal, &status, &sessionID, &ccJSON, &ccID, &exID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan plan: %w", err)
		}
		p.Status = PlanStatus(status)
		if sessionID.Valid {
			p.SessionID = sessionID.String
		}
		if ccJSON.Valid {
			p.CompiledChainJSON = ccJSON.String
		}
		if ccID.Valid {
			p.CompiledChainID = ccID.String
		}
		if exID.Valid {
			p.CompileExecutorChainID = exID.String
		}
		plans = append(plans, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return plans, nil
}

func (s *store) DeletePlan(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM plans
		WHERE id = $1`,
		id,
	)
	if err != nil {
		return fmt.Errorf("failed to delete plan: %w", err)
	}
	return checkRowsAffected(result)
}

// CreatePlanSteps appends new steps.
func (s *store) CreatePlanSteps(ctx context.Context, steps ...*PlanStep) error {
	if len(steps) == 0 {
		return nil
	}

	valueStrings := make([]string, 0, len(steps))
	valueArgs := make([]any, 0, len(steps)*7)

	for i, step := range steps {
		if step.Status == "" {
			step.Status = StepStatusPending
		}
		var execAt sql.NullTime
		if !step.ExecutedAt.IsZero() {
			execAt = sql.NullTime{Time: step.ExecutedAt, Valid: true}
		}

		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			i*7+1, i*7+2, i*7+3, i*7+4, i*7+5, i*7+6, i*7+7))
		valueArgs = append(valueArgs, step.ID, step.PlanID, step.Ordinal, step.Description, string(step.Status), step.ExecutionResult, execAt)
	}

	stmt := fmt.Sprintf(`
		INSERT INTO plan_steps (id, plan_id, ordinal, description, status, execution_result, executed_at)
		VALUES %s`,
		strings.Join(valueStrings, ","),
	)

	_, err := s.Exec.ExecContext(ctx, stmt, valueArgs...)
	if err != nil {
		return fmt.Errorf("failed to create plan steps: %w", err)
	}
	return nil
}

func (s *store) ListPlanSteps(ctx context.Context, planID string) ([]*PlanStep, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, plan_id, ordinal, description, status, execution_result, executed_at
		FROM plan_steps
		WHERE plan_id = $1
		ORDER BY ordinal ASC`,
		planID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query plan steps: %w", err)
	}
	defer rows.Close()

	var steps []*PlanStep
	for rows.Next() {
		var step PlanStep
		var status string
		var execAt sql.NullTime
		if err := rows.Scan(&step.ID, &step.PlanID, &step.Ordinal, &step.Description, &status, &step.ExecutionResult, &execAt); err != nil {
			return nil, fmt.Errorf("failed to scan plan step: %w", err)
		}
		step.Status = StepStatus(status)
		if execAt.Valid {
			step.ExecutedAt = execAt.Time
		}
		steps = append(steps, &step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return steps, nil
}

func (s *store) UpdatePlanStatus(ctx context.Context, planID string, status PlanStatus) error {
	_, err := s.Exec.ExecContext(ctx, `
		UPDATE plans SET status = $1, updated_at = $2 WHERE id = $3`,
		string(status),
		time.Now().UTC(),
		planID,
	)
	if err != nil {
		return fmt.Errorf("failed to update plan status: %w", err)
	}
	return nil
}

func (s *store) UpdatePlanCompiledChain(ctx context.Context, planID string, compiledChainJSON, compiledChainID, executorChainID string) error {
	ccJSON := sql.NullString{String: compiledChainJSON, Valid: compiledChainJSON != ""}
	ccID := sql.NullString{String: compiledChainID, Valid: compiledChainID != ""}
	exID := sql.NullString{String: executorChainID, Valid: executorChainID != ""}
	_, err := s.Exec.ExecContext(ctx, `
		UPDATE plans
		SET compiled_chain_json = $2, compiled_chain_id = $3, compile_executor_chain_id = $4, updated_at = $5
		WHERE id = $1`,
		planID,
		ccJSON,
		ccID,
		exID,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("failed to update plan compiled chain: %w", err)
	}
	return nil
}

func (s *store) UpdatePlanStepStatus(ctx context.Context, stepID string, status StepStatus, result string) error {
	now := time.Now().UTC()
	execAt := sql.NullTime{Time: now, Valid: true}
	// If it's being set back to pending (e.g. from retry), clear the execution time and result
	if status == StepStatusPending {
		execAt = sql.NullTime{Valid: false}
		result = ""
	}

	res, err := s.Exec.ExecContext(ctx, `
		UPDATE plan_steps
		SET status = $2, execution_result = $3, executed_at = $4
		WHERE id = $1`,
		stepID,
		string(status),
		result,
		execAt,
	)
	if err != nil {
		return fmt.Errorf("failed to update step status: %w", err)
	}

	// Also mark the plan as updated_at
	if err := s.touchPlanByStepID(ctx, stepID, now); err != nil {
		return fmt.Errorf("failed to touch plan updated_at: %w", err)
	}

	return checkRowsAffected(res)
}

func (s *store) DeletePendingPlanSteps(ctx context.Context, planID string) error {
	_, err := s.Exec.ExecContext(ctx, "DELETE FROM plan_steps WHERE plan_id = $1 AND status = 'pending'", planID)
	if err != nil {
		return fmt.Errorf("failed to delete pending steps: %w", err)
	}
	return nil
}

// DeleteFinishedPlans removes all completed and archived plans in a single
// statement. Returns the number of plans deleted.
func (s *store) DeleteFinishedPlans(ctx context.Context) (int, error) {
	rows, err := s.Exec.QueryContext(ctx,
		`DELETE FROM plans WHERE status IN ('completed','archived') RETURNING id`)
	if err != nil {
		return 0, fmt.Errorf("failed to delete finished plans: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		count++
	}
	return count, rows.Err()
}

// ArchiveActivePlans sets every active plan's status to 'archived' in one query.
func (s *store) ArchiveActivePlans(ctx context.Context) error {
	_, err := s.Exec.ExecContext(ctx,
		`UPDATE plans SET status = 'archived', updated_at = $1 WHERE status = 'active'`,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("failed to archive active plans: %w", err)
	}
	return nil
}


func (s *store) touchPlanByStepID(ctx context.Context, stepID string, now time.Time) error {
	_, err := s.Exec.ExecContext(ctx, `
		UPDATE plans
		SET updated_at = $2
		WHERE id = (SELECT plan_id FROM plan_steps WHERE id = $1)`,
		stepID,
		now,
	)
	return err
}

func checkRowsAffected(result sql.Result) error {
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
