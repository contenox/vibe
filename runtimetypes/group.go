package runtimetypes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/google/uuid"
)

func (s *store) CreateAffinityGroup(ctx context.Context, group *AffinityGroup) error {
	now := time.Now().UTC()
	group.CreatedAt = now
	group.UpdatedAt = now
	if group.ID == "" {
		group.ID = uuid.New().String()
	}
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO llm_affinity_group
		(id, name, purpose_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)`,
		group.ID, group.Name, group.PurposeType, group.CreatedAt, group.UpdatedAt,
	)
	return err
}

func (s *store) GetAffinityGroup(ctx context.Context, id string) (*AffinityGroup, error) {
	var group AffinityGroup
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, name, purpose_type, created_at, updated_at
		FROM llm_affinity_group WHERE id = $1`, id,
	).Scan(&group.ID, &group.Name, &group.PurposeType, &group.CreatedAt, &group.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, libdb.ErrNotFound
	}
	return &group, err
}

func (s *store) GetAffinityGroupByName(ctx context.Context, name string) (*AffinityGroup, error) {
	var group AffinityGroup
	err := s.Exec.QueryRowContext(ctx, `
		SELECT id, name, purpose_type, created_at, updated_at
		FROM llm_affinity_group WHERE name = $1`, name,
	).Scan(&group.ID, &group.Name, &group.PurposeType, &group.CreatedAt, &group.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, libdb.ErrNotFound
	}
	return &group, err
}

func (s *store) UpdateAffinityGroup(ctx context.Context, group *AffinityGroup) error {
	group.UpdatedAt = time.Now().UTC()

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE llm_affinity_group SET
		name = $2, purpose_type = $3, updated_at = $4
		WHERE id = $1`,
		group.ID, group.Name, group.PurposeType, group.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to update affinity group: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) DeleteAffinityGroup(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM llm_affinity_group WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("failed to delete affinity group: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListAllAffinityGroups(ctx context.Context) ([]*AffinityGroup, error) {
	rows, err := s.Exec.QueryContext(ctx, `
        SELECT id, name, purpose_type, created_at, updated_at
        FROM llm_affinity_group
        ORDER BY created_at DESC, id DESC;
    `)
	if err != nil {
		return nil, fmt.Errorf("failed to query affinity groups: %w", err)
	}
	defer rows.Close()

	groups := []*AffinityGroup{}
	for rows.Next() {
		var group AffinityGroup
		if err := rows.Scan(
			&group.ID,
			&group.Name,
			&group.PurposeType,
			&group.CreatedAt,
			&group.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan affinity group: %w", err)
		}
		groups = append(groups, &group)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return groups, nil
}

func (s *store) ListAffinityGroups(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*AffinityGroup, error) {
	// The cursor is set to the current time if not provided.
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}
	rows, err := s.Exec.QueryContext(ctx, `
        SELECT id, name, purpose_type, created_at, updated_at
        FROM llm_affinity_group
        WHERE created_at < $1
        ORDER BY created_at DESC, id DESC
        LIMIT $2`,
		cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query affinity groups: %w", err)
	}
	defer rows.Close()

	var groups []*AffinityGroup
	for rows.Next() {
		var group AffinityGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.PurposeType, &group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan affinity group: %w", err)
		}
		groups = append(groups, &group)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	if groups == nil {
		return []*AffinityGroup{}, nil
	}
	return groups, nil
}

// ListAffinityGroupByPurpose retrieves a list of LLM affinity groups for a specific purpose,
// created before the provided cursor, ordered from newest to oldest.
func (s *store) ListAffinityGroupByPurpose(ctx context.Context, purposeType string, createdAtCursor *time.Time, limit int) ([]*AffinityGroup, error) {
	// The cursor is set to the current time if not provided.
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}

	rows, err := s.Exec.QueryContext(ctx, `
        SELECT id, name, purpose_type, created_at, updated_at
        FROM llm_affinity_group WHERE purpose_type = $1 AND created_at < $2
        ORDER BY created_at DESC, id DESC
        LIMIT $3`,
		purposeType, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query affinity groups by purpose: %w", err)
	}
	defer rows.Close()

	var groups []*AffinityGroup
	for rows.Next() {
		var group AffinityGroup
		if err := rows.Scan(&group.ID, &group.Name, &group.PurposeType, &group.CreatedAt, &group.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan affinity group: %w", err)
		}
		groups = append(groups, &group)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	if groups == nil {
		return []*AffinityGroup{}, nil
	}
	return groups, nil
}

func (s *store) AssignBackendToAffinityGroup(ctx context.Context, groupID, backendID string) error {
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO llm_affinity_group_backend_assignments
		(group_id, backend_id, assigned_at)
		VALUES ($1, $2, $3)`,
		groupID, backendID, time.Now().UTC())
	return err
}

func (s *store) RemoveBackendFromAffinityGroup(ctx context.Context, groupID, backendID string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM llm_affinity_group_backend_assignments
		WHERE group_id = $1 AND backend_id = $2`, groupID, backendID)
	if err != nil {
		return fmt.Errorf("failed to remove backend from affinity group: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListBackendsForAffinityGroup(ctx context.Context, groupID string) ([]*Backend, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT b.id, b.name, b.base_url, b.type, b.created_at, b.updated_at
		FROM llm_backends b
		INNER JOIN llm_affinity_group_backend_assignments a ON b.id = a.backend_id
		WHERE a.group_id = $1
		ORDER BY a.assigned_at DESC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to list backends for affinity group: %w", err)
	}
	defer rows.Close()

	var backends []*Backend
	for rows.Next() {
		var b Backend
		if err := rows.Scan(&b.ID, &b.Name, &b.BaseURL, &b.Type, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan backend: %w", err)
		}
		backends = append(backends, &b)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if backends == nil {
		return []*Backend{}, nil
	}
	return backends, nil
}

func (s *store) ListAffinityGroupsForBackend(ctx context.Context, backendID string) ([]*AffinityGroup, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT p.id, p.name, p.purpose_type, p.created_at, p.updated_at
		FROM llm_affinity_group p
		INNER JOIN llm_affinity_group_backend_assignments a ON p.id = a.group_id
		WHERE a.backend_id = $1
		ORDER BY a.assigned_at DESC`, backendID)
	if err != nil {
		return nil, fmt.Errorf("failed to list affinity groups for backend: %w", err)
	}
	defer rows.Close()

	var groups []*AffinityGroup
	for rows.Next() {
		var p AffinityGroup
		if err := rows.Scan(&p.ID, &p.Name, &p.PurposeType, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan affinity group: %w", err)
		}
		groups = append(groups, &p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if groups == nil {
		return []*AffinityGroup{}, nil
	}
	return groups, nil
}

func (s *store) AssignModelToAffinityGroup(ctx context.Context, groupID, modelID string) error {
	now := time.Now().UTC()
	_, err := s.Exec.ExecContext(ctx, `
		INSERT INTO ollama_model_assignments
		(model_id, llm_group_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4)`, modelID, groupID, now, now)
	return err
}

func (s *store) RemoveModelFromAffinityGroup(ctx context.Context, groupID, modelID string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM ollama_model_assignments
		WHERE model_id = $1 AND llm_group_id = $2`, modelID, groupID)
	if err != nil {
		return fmt.Errorf("failed to remove model from affinity group: %w", err)
	}
	return checkRowsAffected(result)
}

func (s *store) ListModelsForAffinityGroup(ctx context.Context, groupID string) ([]*Model, error) {
	rows, err := s.Exec.QueryContext(ctx, `
        SELECT m.id, m.model, m.context_length, m.can_chat, m.can_embed, m.can_prompt, m.can_stream, m.created_at, m.updated_at
        FROM ollama_models m
        INNER JOIN ollama_model_assignments a ON m.id = a.model_id
        WHERE a.llm_group_id = $1
        ORDER BY a.created_at DESC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to list models for affinity group: %w", err)
	}
	defer rows.Close()

	var models []*Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(
			&m.ID,
			&m.Model,
			&m.ContextLength,
			&m.CanChat,
			&m.CanEmbed,
			&m.CanPrompt,
			&m.CanStream,
			&m.CreatedAt,
			&m.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan model: %w", err)
		}
		models = append(models, &m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if models == nil {
		return []*Model{}, nil
	}
	return models, nil
}

func (s *store) ListAffinityGroupsForModel(ctx context.Context, modelID string) ([]*AffinityGroup, error) {
	rows, err := s.Exec.QueryContext(ctx, `
		SELECT p.id, p.name, p.purpose_type, p.created_at, p.updated_at
		FROM llm_affinity_group p
		INNER JOIN ollama_model_assignments a ON p.id = a.llm_group_id
		WHERE a.model_id = $1
		ORDER BY a.created_at DESC`, modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to list affinity groups for model: %w", err)
	}
	defer rows.Close()

	var groups []*AffinityGroup
	for rows.Next() {
		var p AffinityGroup
		if err := rows.Scan(&p.ID, &p.Name, &p.PurposeType, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan affinity group: %w", err)
		}
		groups = append(groups, &p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	if groups == nil {
		return []*AffinityGroup{}, nil
	}
	return groups, nil
}

func (s *store) EstimateAffinityGroupCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "llm_affinity_group")
}
