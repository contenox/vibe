package runtimetypes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/google/uuid"
)

type ModelRegistryEntry struct {
	ID        string    `json:"id" example:"m1a2b3c4-d5e6-f7g8-h9i0-j1k2l3m4n5o6"`
	Name      string    `json:"name" example:"qwen2.5-1.5b"`
	SourceURL string    `json:"sourceUrl" example:"https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf"`
	SizeBytes int64     `json:"sizeBytes" example:"934000000"`
	CreatedAt time.Time `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt time.Time `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

func (s *store) CreateModelRegistryEntry(ctx context.Context, e *ModelRegistryEntry) error {
	now := time.Now().UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO llm_model_registry
		(id, name, source_url, size_bytes, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.ID, e.Name, e.SourceURL, e.SizeBytes, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (s *store) GetModelRegistryEntry(ctx context.Context, id string) (*ModelRegistryEntry, error) {
	var e ModelRegistryEntry
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, name, source_url, size_bytes, created_at, updated_at
		FROM llm_model_registry
		WHERE id = $1`, id,
	).Scan(&e.ID, &e.Name, &e.SourceURL, &e.SizeBytes, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, libdb.ErrNotFound
	}
	return &e, err
}

func (s *store) GetModelRegistryEntryByName(ctx context.Context, name string) (*ModelRegistryEntry, error) {
	var e ModelRegistryEntry
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, name, source_url, size_bytes, created_at, updated_at
		FROM llm_model_registry
		WHERE name = $1`, name,
	).Scan(&e.ID, &e.Name, &e.SourceURL, &e.SizeBytes, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, libdb.ErrNotFound
	}
	return &e, err
}

func (s *store) UpdateModelRegistryEntry(ctx context.Context, e *ModelRegistryEntry) error {
	e.UpdatedAt = time.Now().UTC()
	result, err := s.Exec.ExecContext(ctx, `
		UPDATE llm_model_registry
		SET name = $2, source_url = $3, size_bytes = $4, updated_at = $5
		WHERE id = $1`,
		e.ID, e.Name, e.SourceURL, e.SizeBytes, e.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to update model registry entry: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) DeleteModelRegistryEntry(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM llm_model_registry WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("failed to delete model registry entry: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListModelRegistryEntries(ctx context.Context, cursor *time.Time, limit int) ([]*ModelRegistryEntry, error) {
	t := time.Now().UTC()
	if cursor != nil {
		t = *cursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, name, source_url, size_bytes, created_at, updated_at
		FROM llm_model_registry
		WHERE created_at < $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, t, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query model registry: %w", err)
	}
	defer rows.Close()

	var out []*ModelRegistryEntry
	for rows.Next() {
		var e ModelRegistryEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.SourceURL, &e.SizeBytes, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan model registry entry: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return out, nil
}

func (s *store) EstimateModelRegistryEntryCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "llm_model_registry")
}
