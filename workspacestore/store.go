package workspacestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
)

var ErrNotFound = errors.New("workspace not found")

type store struct {
	Exec libdbexec.Exec
}

func New(exec libdbexec.Exec) Store {
	return &store{Exec: exec}
}

func (s *store) Insert(ctx context.Context, w *Workspace) error {
	now := time.Now().UTC()
	if w.CreatedAt.IsZero() {
		w.CreatedAt = now
	}
	if w.UpdatedAt.IsZero() {
		w.UpdatedAt = now
	}
	var shell any
	if w.Shell != "" {
		shell = w.Shell
	} else {
		shell = nil
	}
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO workspaces (id, principal, name, path, shell, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID, w.Principal, w.Name, w.Path, shell, w.CreatedAt, w.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("workspacestore: insert: %w", err)
	}
	return nil
}

func (s *store) GetByID(ctx context.Context, id string) (*Workspace, error) {
	return s.getByCondition(ctx, "id = $1", id)
}

func (s *store) GetByIDAndPrincipal(ctx context.Context, id, principal string) (*Workspace, error) {
	return s.getByCondition(ctx, "id = $1 AND principal = $2", id, principal)
}

func (s *store) getByCondition(ctx context.Context, condition string, args ...any) (*Workspace, error) {
	var w Workspace
	var shell sql.NullString
	q := fmt.Sprintf(`
		SELECT id, principal, name, path, shell, created_at, updated_at
		FROM workspaces WHERE %s`, condition)
	err := s.Exec.QueryRowContext(ctx, q, args...).Scan(
		&w.ID, &w.Principal, &w.Name, &w.Path, &shell, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workspacestore: get: %w", err)
	}
	if shell.Valid {
		w.Shell = shell.String
	}
	return &w, nil
}

func (s *store) ListByPrincipal(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*Workspace, error) {
	if limit > runtimetypes.MAXLIMIT {
		return nil, runtimetypes.ErrLimitParamExceeded
	}
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, principal, name, path, shell, created_at, updated_at
		FROM workspaces
		WHERE principal = $1 AND created_at < $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`,
		principal, cursor, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("workspacestore: list: %w", err)
	}
	defer rows.Close()

	out := make([]*Workspace, 0)
	for rows.Next() {
		var w Workspace
		var shell sql.NullString
		if err := rows.Scan(&w.ID, &w.Principal, &w.Name, &w.Path, &shell, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("workspacestore: list scan: %w", err)
		}
		if shell.Valid {
			w.Shell = shell.String
		}
		out = append(out, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspacestore: list rows: %w", err)
	}
	return out, nil
}

func (s *store) Update(ctx context.Context, w *Workspace) error {
	now := time.Now().UTC()
	w.UpdatedAt = now
	var shell any
	if w.Shell != "" {
		shell = w.Shell
	} else {
		shell = nil
	}
	res, err := s.Exec.ExecContext(ctx, `
		UPDATE workspaces SET name = $1, path = $2, shell = $3, updated_at = $4 WHERE id = $5 AND principal = $6`,
		w.Name, w.Path, shell, w.UpdatedAt, w.ID, w.Principal,
	)
	if err != nil {
		return fmt.Errorf("workspacestore: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workspacestore: update rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *store) DeleteByIDAndPrincipal(ctx context.Context, id, principal string) error {
	res, err := s.Exec.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1 AND principal = $2`, id, principal)
	if err != nil {
		return fmt.Errorf("workspacestore: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workspacestore: delete rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
