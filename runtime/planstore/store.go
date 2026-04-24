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
	Exec        libdbexec.Exec
	workspaceID string
}

func New(exec libdbexec.Exec, workspaceID string) Store {
	return &store{Exec: exec, workspaceID: workspaceID}
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
	rcJSON := sql.NullString{String: plan.RepoContextJSON, Valid: plan.RepoContextJSON != ""}

	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO plans (id, name, workspace_id, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, repo_context_json, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		plan.ID,
		plan.Name,
		s.workspaceID,
		plan.Goal,
		string(plan.Status),
		sessionID,
		ccJSON,
		ccID,
		exID,
		rcJSON,
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
	var ccJSON, ccID, exID, rcJSON sql.NullString
	var status string

	query := fmt.Sprintf(`
		SELECT id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, repo_context_json, created_at, updated_at
		FROM plans
		WHERE workspace_id = $2 AND %s`, condition)

	err := s.Exec.QueryRowContext(ctx, query, arg, s.workspaceID).Scan(
		&p.ID,
		&p.Name,
		&p.Goal,
		&status,
		&sessionID,
		&ccJSON,
		&ccID,
		&exID,
		&rcJSON,
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
	if rcJSON.Valid {
		p.RepoContextJSON = rcJSON.String
	}
	return &p, nil
}

// GetActivePlan returns the most recently updated active plan.
func (s *store) GetActivePlan(ctx context.Context) (*Plan, error) {
	var p Plan
	var sessionID sql.NullString
	var status string
	var ccJSON, ccID, exID, rcJSON sql.NullString
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, repo_context_json, created_at, updated_at
		FROM plans
		WHERE workspace_id = $1 AND status = 'active'
		ORDER BY updated_at DESC
		LIMIT 1`,
		s.workspaceID,
	).Scan(&p.ID, &p.Name, &p.Goal, &status, &sessionID, &ccJSON, &ccID, &exID, &rcJSON, &p.CreatedAt, &p.UpdatedAt)
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
	if rcJSON.Valid {
		p.RepoContextJSON = rcJSON.String
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
		SELECT id, name, goal, status, session_id, compiled_chain_json, compiled_chain_id, compile_executor_chain_id, repo_context_json, created_at, updated_at
		FROM plans
		WHERE workspace_id = $1
		ORDER BY created_at ASC`,
		s.workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query plans: %w", err)
	}
	defer rows.Close()

	var plans []*Plan
	for rows.Next() {
		var p Plan
		var sessionID sql.NullString
		var ccJSON, ccID, exID, rcJSON sql.NullString
		var status string
		if err := rows.Scan(&p.ID, &p.Name, &p.Goal, &status, &sessionID, &ccJSON, &ccID, &exID, &rcJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
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
		if rcJSON.Valid {
			p.RepoContextJSON = rcJSON.String
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
		WHERE id = $1 AND workspace_id = $2`,
		id, s.workspaceID,
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
		SELECT id, plan_id, ordinal, description, status, execution_result, executed_at,
		       summary, chat_history_json, summary_error, last_failure_summary, failure_class
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
		var summary, chatHist, summaryErr, lastFail, failureClass sql.NullString
		if err := rows.Scan(&step.ID, &step.PlanID, &step.Ordinal, &step.Description, &status, &step.ExecutionResult, &execAt,
			&summary, &chatHist, &summaryErr, &lastFail, &failureClass); err != nil {
			return nil, fmt.Errorf("failed to scan plan step: %w", err)
		}
		step.Status = StepStatus(status)
		if execAt.Valid {
			step.ExecutedAt = execAt.Time
		}
		if summary.Valid {
			step.Summary = summary.String
		}
		if chatHist.Valid {
			step.ChatHistoryJSON = chatHist.String
		}
		if summaryErr.Valid {
			step.SummaryError = summaryErr.String
		}
		if lastFail.Valid {
			step.LastFailureSummary = lastFail.String
		}
		if failureClass.Valid {
			step.FailureClass = FailureClass(failureClass.String)
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

func (s *store) SetPlanStepFailureClass(ctx context.Context, stepID string, class FailureClass) error {
	cls := sql.NullString{String: string(class), Valid: class != FailureClassEmpty}
	_, err := s.Exec.ExecContext(ctx, `
		UPDATE plan_steps SET failure_class = $2 WHERE id = $1`,
		stepID,
		cls,
	)
	if err != nil {
		return fmt.Errorf("failed to set plan step failure_class: %w", err)
	}
	return nil
}

func (s *store) UpdatePlanRepoContext(ctx context.Context, planID string, repoContextJSON string) error {
	rcJSON := sql.NullString{String: repoContextJSON, Valid: repoContextJSON != ""}
	_, err := s.Exec.ExecContext(ctx, `
		UPDATE plans
		SET repo_context_json = $2, updated_at = $3
		WHERE id = $1`,
		planID,
		rcJSON,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("failed to update plan repo context: %w", err)
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

// UpdatePlanStepSummary persists the typed summary JSON + raw executor chat history.
// Called by the plan_summary persist hook when summarizer output validated successfully.
func (s *store) UpdatePlanStepSummary(ctx context.Context, stepID string, summaryJSON, chatHistoryJSON string) error {
	summary := sql.NullString{String: summaryJSON, Valid: summaryJSON != ""}
	chatHist := sql.NullString{String: chatHistoryJSON, Valid: chatHistoryJSON != ""}
	now := time.Now().UTC()
	res, err := s.Exec.ExecContext(ctx, `
		UPDATE plan_steps
		SET summary = $2, chat_history_json = $3, summary_error = NULL
		WHERE id = $1`,
		stepID, summary, chatHist,
	)
	if err != nil {
		return fmt.Errorf("failed to update plan step summary: %w", err)
	}
	if err := s.touchPlanByStepID(ctx, stepID, now); err != nil {
		return fmt.Errorf("failed to touch plan updated_at: %w", err)
	}
	return checkRowsAffected(res)
}

// UpdatePlanStepSummaryFailure records that summarizer validation failed twice.
// Persists the raw output + error alongside a fallback ExecutionResult string so
// the next step still has human-readable context.
func (s *store) UpdatePlanStepSummaryFailure(ctx context.Context, stepID string, rawOutput, errMsg, fallbackExecResult string) error {
	payload := strings.TrimSpace(errMsg)
	if rawOutput != "" {
		payload = payload + "\n\n--- raw summarizer output ---\n" + rawOutput
	}
	errField := sql.NullString{String: payload, Valid: payload != ""}
	now := time.Now().UTC()
	res, err := s.Exec.ExecContext(ctx, `
		UPDATE plan_steps
		SET summary = NULL, summary_error = $2, execution_result = $3
		WHERE id = $1`,
		stepID, errField, fallbackExecResult,
	)
	if err != nil {
		return fmt.Errorf("failed to update plan step summary failure: %w", err)
	}
	if err := s.touchPlanByStepID(ctx, stepID, now); err != nil {
		return fmt.Errorf("failed to touch plan updated_at: %w", err)
	}
	return checkRowsAffected(res)
}

// MoveSummaryToLastFailure copies current Summary (or ExecutionResult fallback) into
// LastFailureSummary and clears the summary columns. Called by Retry so the re-run's
// summarizer can see why the prior attempt failed.
func (s *store) MoveSummaryToLastFailure(ctx context.Context, stepID string) error {
	_, err := s.Exec.ExecContext(ctx, `
		UPDATE plan_steps
		SET last_failure_summary = COALESCE(NULLIF(summary, ''), NULLIF(execution_result, '')),
		    summary = NULL,
		    chat_history_json = NULL,
		    summary_error = NULL,
		    failure_class = NULL
		WHERE id = $1`,
		stepID,
	)
	if err != nil {
		return fmt.Errorf("failed to move summary to last failure: %w", err)
	}
	return nil
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
		`DELETE FROM plans WHERE workspace_id = $1 AND status IN ('completed','archived') RETURNING id`,
		s.workspaceID,
	)
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
		`UPDATE plans SET status = 'archived', updated_at = $1 WHERE workspace_id = $2 AND status = 'active'`,
		time.Now().UTC(), s.workspaceID,
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
