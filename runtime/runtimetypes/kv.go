package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

func (s *store) SetKV(ctx context.Context, key string, value json.RawMessage) error {
	return s.setKVScoped(ctx, "", key, value)
}

func (s *store) SetWorkspaceKV(ctx context.Context, workspaceID string, key string, value json.RawMessage) error {
	return s.setKVScoped(ctx, workspaceID, key, value)
}

func (s *store) setKVScoped(ctx context.Context, workspaceID string, key string, value json.RawMessage) error {
	now := time.Now().UTC()
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO kv (key, workspace_id, value, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key, workspace_id) DO UPDATE
		SET value = $3, updated_at = $5`,
		key, workspaceID, value, now, now,
	)
	return err
}

func (s *store) UpdateKV(ctx context.Context, key string, value json.RawMessage) error {
	now := time.Now().UTC()
	result, err := s.Exec.ExecContext(ctx, `
        UPDATE kv
        SET value = $2, updated_at = $3
        WHERE key = $1 AND workspace_id = ''`,
		key, value, now,
	)
	if err != nil {
		return fmt.Errorf("failed to update key-value pair: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) GetKV(ctx context.Context, key string, out interface{}) error {
	return s.getKVScoped(ctx, "", key, out)
}

func (s *store) GetWorkspaceKV(ctx context.Context, workspaceID string, key string, out interface{}) error {
	return s.getKVScoped(ctx, workspaceID, key, out)
}

func (s *store) getKVScoped(ctx context.Context, workspaceID string, key string, out interface{}) error {
	var kv KV
	err := s.Exec.QueryRowContext(ctx, `
		SELECT key, value, created_at, updated_at
		FROM kv
		WHERE key = $1 AND workspace_id = $2`,
		key, workspaceID,
	).Scan(&kv.Key, &kv.Value, &kv.CreatedAt, &kv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return libdb.ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(kv.Value, out)
}

func (s *store) DeleteKV(ctx context.Context, key string) error {
	return s.deleteKVScoped(ctx, "", key)
}

func (s *store) DeleteWorkspaceKV(ctx context.Context, workspaceID string, key string) error {
	return s.deleteKVScoped(ctx, workspaceID, key)
}

func (s *store) deleteKVScoped(ctx context.Context, workspaceID string, key string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM kv
		WHERE key = $1 AND workspace_id = $2`,
		key, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("failed to delete key-value pair: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListKV(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*KV, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	rows, err := s.Exec.QueryContext(ctx, `
        SELECT key, value, created_at, updated_at
        FROM kv
        WHERE workspace_id = '' AND created_at < $1
        ORDER BY created_at DESC, key DESC
        LIMIT $2;
    `, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query key-value pairs: %w", err)
	}
	defer rows.Close()

	var kvs []*KV
	for rows.Next() {
		var kv KV
		if err := rows.Scan(&kv.Key, &kv.Value, &kv.CreatedAt, &kv.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan key-value pair: %w", err)
		}
		kvs = append(kvs, &kv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return kvs, nil
}

func (s *store) ListKVPrefix(ctx context.Context, prefix string, createdAtCursor *time.Time, limit int) ([]*KV, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	rows, err := s.Exec.QueryContext(ctx, `
        SELECT key, value, created_at, updated_at
        FROM kv
        WHERE workspace_id = '' AND key LIKE $1 || '%' AND created_at < $2
        ORDER BY created_at DESC, key DESC
        LIMIT $3;
    `, prefix, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query key-value pairs with prefix: %w", err)
	}
	defer rows.Close()

	var kvs []*KV
	for rows.Next() {
		var kv KV
		if err := rows.Scan(&kv.Key, &kv.Value, &kv.CreatedAt, &kv.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan key-value pair: %w", err)
		}
		kvs = append(kvs, &kv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return kvs, nil
}

func (s *store) EstimateKVCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "kv")
}
