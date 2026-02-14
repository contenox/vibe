package functionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/vibe/libdbexec"
)

var _ Store = (*store)(nil)

// store implements Store using libdbexec
type store struct {
	libdbexec.Exec
}

// New creates a new function store instance
func New(exec libdbexec.Exec) Store {
	return &store{Exec: exec}
}

func (s *store) EnforceMaxRowCount(ctx context.Context, count int64) error {
	if count >= MAXLIMIT {
		return fmt.Errorf("row limit reached (max %d)", MAXLIMIT)
	}
	return nil
}

// Function management methods
func (s *store) CreateFunction(ctx context.Context, function *Function) error {
	now := time.Now().UTC()
	function.CreatedAt = now
	function.UpdatedAt = now

	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO functions
		(name, description, script_type, script, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		function.Name, function.Description, function.ScriptType, function.Script,
		function.CreatedAt, function.UpdatedAt,
	)
	return err
}

func (s *store) GetFunction(ctx context.Context, name string) (*Function, error) {
	var function Function
	err := s.Exec.QueryRowContext(ctx, `
		SELECT name, description, script_type, script, created_at, updated_at
		FROM functions WHERE name = $1`, name,
	).Scan(
		&function.Name, &function.Description, &function.ScriptType, &function.Script,
		&function.CreatedAt, &function.UpdatedAt,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &function, err
}

func (s *store) UpdateFunction(ctx context.Context, function *Function) error {
	function.UpdatedAt = time.Now().UTC()

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE functions SET
		description = $2, script_type = $3, script = $4, updated_at = $5
		WHERE name = $1`,
		function.Name, function.Description, function.ScriptType, function.Script,
		function.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to update function: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) DeleteFunction(ctx context.Context, name string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM functions WHERE name = $1`, name,
	)
	if err != nil {
		return fmt.Errorf("failed to delete function: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListFunctions(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*Function, error) {
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}

	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}

	rows, err := s.Exec.QueryContext(ctx, `
		SELECT name, description, script_type, script, created_at, updated_at
		FROM functions WHERE created_at < $1
		ORDER BY created_at DESC
		LIMIT $2`,
		cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query functions: %w", err)
	}
	defer rows.Close()

	var functions []*Function
	for rows.Next() {
		var f Function
		if err := rows.Scan(
			&f.Name, &f.Description, &f.ScriptType, &f.Script,
			&f.CreatedAt, &f.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan function: %w", err)
		}
		functions = append(functions, &f)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	if functions == nil {
		return []*Function{}, nil
	}
	return functions, nil
}

func (s *store) ListAllFunctions(ctx context.Context) ([]*Function, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT name, description, script_type, script, created_at, updated_at
		FROM functions ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query functions: %w", err)
	}
	defer rows.Close()

	var functions []*Function
	for rows.Next() {
		var f Function
		if err := rows.Scan(
			&f.Name, &f.Description, &f.ScriptType, &f.Script,
			&f.CreatedAt, &f.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan function: %w", err)
		}
		functions = append(functions, &f)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if functions == nil {
		return []*Function{}, nil
	}
	return functions, nil
}

// Event trigger management methods
func (s *store) CreateEventTrigger(ctx context.Context, trigger *EventTrigger) error {
	now := time.Now().UTC()
	trigger.CreatedAt = now
	trigger.UpdatedAt = now

	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO event_triggers
		(name, description, listen_for_type, trigger_type, function_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		trigger.Name, trigger.Description, trigger.ListenFor.Type,
		trigger.Type, trigger.Function, trigger.CreatedAt, trigger.UpdatedAt,
	)
	return err
}

func (s *store) GetEventTrigger(ctx context.Context, name string) (*EventTrigger, error) {
	var trigger EventTrigger
	err := s.Exec.QueryRowContext(ctx, `
		SELECT name, description, listen_for_type, trigger_type, function_name, created_at, updated_at
		FROM event_triggers WHERE name = $1`, name,
	).Scan(
		&trigger.Name, &trigger.Description, &trigger.ListenFor.Type,
		&trigger.Type, &trigger.Function, &trigger.CreatedAt, &trigger.UpdatedAt,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &trigger, err
}

func (s *store) UpdateEventTrigger(ctx context.Context, trigger *EventTrigger) error {
	trigger.UpdatedAt = time.Now().UTC()

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE event_triggers SET
		description = $2, listen_for_type = $3, trigger_type = $4, function_name = $5, updated_at = $6
		WHERE name = $1`,
		trigger.Name, trigger.Description, trigger.ListenFor.Type,
		trigger.Type, trigger.Function, trigger.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to update event trigger: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) DeleteEventTrigger(ctx context.Context, name string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM event_triggers WHERE name = $1`, name,
	)
	if err != nil {
		return fmt.Errorf("failed to delete event trigger: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListEventTriggers(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*EventTrigger, error) {
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}

	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}

	rows, err := s.Exec.QueryContext(ctx, `
		SELECT name, description, listen_for_type, trigger_type, function_name, created_at, updated_at
		FROM event_triggers WHERE created_at < $1
		ORDER BY created_at DESC
		LIMIT $2`,
		cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query event triggers: %w", err)
	}
	defer rows.Close()

	var triggers []*EventTrigger
	for rows.Next() {
		var t EventTrigger
		if err := rows.Scan(
			&t.Name, &t.Description, &t.ListenFor.Type,
			&t.Type, &t.Function, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan event trigger: %w", err)
		}
		triggers = append(triggers, &t)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	if triggers == nil {
		return []*EventTrigger{}, nil
	}
	return triggers, nil
}

func (s *store) ListAllEventTriggers(ctx context.Context) ([]*EventTrigger, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT name, description, listen_for_type, trigger_type, function_name, created_at, updated_at
		FROM event_triggers ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query event triggers: %w", err)
	}
	defer rows.Close()

	var triggers []*EventTrigger
	for rows.Next() {
		var t EventTrigger
		if err := rows.Scan(
			&t.Name, &t.Description, &t.ListenFor.Type,
			&t.Type, &t.Function, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan event trigger: %w", err)
		}
		triggers = append(triggers, &t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if triggers == nil {
		return []*EventTrigger{}, nil
	}
	return triggers, nil
}

func (s *store) ListEventTriggersByEventType(ctx context.Context, eventType string) ([]*EventTrigger, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT name, description, listen_for_type, trigger_type, function_name, created_at, updated_at
		FROM event_triggers WHERE listen_for_type = $1
		ORDER BY created_at DESC`,
		eventType)
	if err != nil {
		return nil, fmt.Errorf("failed to query event triggers by event type: %w", err)
	}
	defer rows.Close()

	var triggers []*EventTrigger
	for rows.Next() {
		var t EventTrigger
		if err := rows.Scan(
			&t.Name, &t.Description, &t.ListenFor.Type,
			&t.Type, &t.Function, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan event trigger: %w", err)
		}
		triggers = append(triggers, &t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if triggers == nil {
		return []*EventTrigger{}, nil
	}
	return triggers, nil
}

func (s *store) ListEventTriggersByFunction(ctx context.Context, functionName string) ([]*EventTrigger, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT name, description, listen_for_type, trigger_type, function_name, created_at, updated_at
		FROM event_triggers WHERE function_name = $1
		ORDER BY created_at DESC`,
		functionName)
	if err != nil {
		return nil, fmt.Errorf("failed to query event triggers by function: %w", err)
	}
	defer rows.Close()

	var triggers []*EventTrigger
	for rows.Next() {
		var t EventTrigger
		if err := rows.Scan(
			&t.Name, &t.Description, &t.ListenFor.Type,
			&t.Type, &t.Function, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan event trigger: %w", err)
		}
		triggers = append(triggers, &t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if triggers == nil {
		return []*EventTrigger{}, nil
	}
	return triggers, nil
}

// Helper methods
func (s *store) EstimateFunctionCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "functions")
}

func (s *store) EstimateEventTriggerCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "event_triggers")
}

func (s *store) estimateCount(ctx context.Context, table string) (int64, error) {
	var count int64
	err := s.Exec.QueryRowContext(ctx, `
		SELECT estimate_row_count($1)
	`, table).Scan(&count)
	return count, err
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
