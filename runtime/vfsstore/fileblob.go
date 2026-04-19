package vfsstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
)

func (s *store) CreateBlob(ctx context.Context, blob *Blob) error {
	now := time.Now().UTC()
	blob.CreatedAt = now
	blob.UpdatedAt = now
	_, err := s.Exec.ExecContext(ctx, `
        INSERT INTO vfs_blobs (id, meta, data, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5)`,
		blob.ID, blob.Meta, blob.Data, blob.CreatedAt, blob.UpdatedAt)
	return err
}

func (s *store) GetBlobByID(ctx context.Context, id string) (*Blob, error) {
	var b Blob
	err := s.Exec.QueryRowContext(ctx, `
        SELECT id, meta, data, created_at, updated_at
        FROM vfs_blobs
        WHERE id = $1`,
		id,
	).Scan(&b.ID, &b.Meta, &b.Data, &b.CreatedAt, &b.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, libdb.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get blob failed: %w", err)
	}
	return &b, nil
}

func (s *store) DeleteBlob(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `DELETE FROM vfs_blobs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete blob failed: %w", err)
	}
	return checkRowsAffected(result)
}

// UpdateBlob updates blob data and meta in-place (Fix 6).
// Use this instead of DeleteBlob+CreateBlob to avoid FK violations when adding
// foreign key constraints on vfs_files.blobs_id → vfs_blobs.id.
func (s *store) UpdateBlob(ctx context.Context, id string, data, meta []byte) error {
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE vfs_blobs SET data = $2, meta = $3, updated_at = $4 WHERE id = $1`,
		id, data, meta, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("update blob failed: %w", err)
	}
	return checkRowsAffected(result)
}
