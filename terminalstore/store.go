package terminalstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
)

var ErrNotFound = errors.New("terminal session not found")

type store struct {
	Exec libdbexec.Exec
}

// New constructs a Store backed by exec (use db.WithoutTransaction() or a tx from WithTransaction).
func New(exec libdbexec.Exec) Store {
	return &store{Exec: exec}
}

func (s *store) Insert(ctx context.Context, sess *Session) error {
	now := time.Now().UTC()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = now
	}
	if sess.Status == "" {
		sess.Status = SessionStatusActive
	}
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO terminal_sessions (id, principal, cwd, shell, cols, rows, status, node_instance_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		sess.ID,
		sess.Principal,
		sess.CWD,
		sess.Shell,
		sess.Cols,
		sess.Rows,
		string(sess.Status),
		sess.NodeInstanceID,
		sess.CreatedAt,
		sess.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("terminalstore: insert: %w", err)
	}
	return nil
}

func (s *store) GetByID(ctx context.Context, id string) (*Session, error) {
	return s.getByCondition(ctx, "id = $1", id)
}

func (s *store) GetByIDAndPrincipal(ctx context.Context, id, principal string) (*Session, error) {
	return s.getByCondition(ctx, "id = $1 AND principal = $2", id, principal)
}

func (s *store) getByCondition(ctx context.Context, condition string, args ...any) (*Session, error) {
	var sess Session
	var status string
	q := fmt.Sprintf(`
		SELECT id, principal, cwd, shell, cols, rows, status, node_instance_id, created_at, updated_at
		FROM terminal_sessions
		WHERE %s`, condition)
	err := s.Exec.QueryRowContext(ctx, q, args...).Scan(
		&sess.ID,
		&sess.Principal,
		&sess.CWD,
		&sess.Shell,
		&sess.Cols,
		&sess.Rows,
		&status,
		&sess.NodeInstanceID,
		&sess.CreatedAt,
		&sess.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("terminalstore: get: %w", err)
	}
	sess.Status = SessionStatus(status)
	return &sess, nil
}

func (s *store) ListByPrincipal(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*Session, error) {
	if limit > runtimetypes.MAXLIMIT {
		return nil, runtimetypes.ErrLimitParamExceeded
	}
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, principal, cwd, shell, cols, rows, status, node_instance_id, created_at, updated_at
		FROM terminal_sessions
		WHERE principal = $1 AND status = $2 AND created_at < $3
		ORDER BY created_at DESC, id DESC
		LIMIT $4`,
		principal, string(SessionStatusActive), cursor, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("terminalstore: list: %w", err)
	}
	defer rows.Close()

	out := make([]*Session, 0)
	for rows.Next() {
		var sess Session
		var status string
		if err := rows.Scan(
			&sess.ID,
			&sess.Principal,
			&sess.CWD,
			&sess.Shell,
			&sess.Cols,
			&sess.Rows,
			&status,
			&sess.NodeInstanceID,
			&sess.CreatedAt,
			&sess.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("terminalstore: list scan: %w", err)
		}
		sess.Status = SessionStatus(status)
		out = append(out, &sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("terminalstore: list rows: %w", err)
	}
	return out, nil
}

func (s *store) UpdateGeometry(ctx context.Context, id string, cols, rows int) error {
	now := time.Now().UTC()
	res, err := s.Exec.ExecContext(ctx, `
		UPDATE terminal_sessions SET cols = $1, rows = $2, updated_at = $3 WHERE id = $4 AND status = $5`,
		cols, rows, now, id, string(SessionStatusActive),
	)
	if err != nil {
		return fmt.Errorf("terminalstore: update geometry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("terminalstore: update geometry rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *store) Delete(ctx context.Context, id string) error {
	res, err := s.Exec.ExecContext(ctx, `DELETE FROM terminal_sessions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("terminalstore: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("terminalstore: delete rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *store) DeleteByNodeInstanceID(ctx context.Context, nodeInstanceID string) error {
	_, err := s.Exec.ExecContext(ctx, `DELETE FROM terminal_sessions WHERE node_instance_id = $1`, nodeInstanceID)
	if err != nil {
		return fmt.Errorf("terminalstore: delete by node: %w", err)
	}
	return nil
}
