package runtimetypes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/google/uuid"
)

// encodeProperties serializes a map into a byte slice using gob.
func encodeProperties(props InjectionArg) ([]byte, error) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if err := encoder.Encode(props); err != nil {
		return nil, fmt.Errorf("failed to gob-encode body properties: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeProperties deserializes a byte slice into a map using gob.
func decodeProperties(data []byte) (InjectionArg, error) {
	if len(data) == 0 {
		return InjectionArg{}, nil
	}
	var props InjectionArg
	reader := bytes.NewReader(data)
	decoder := gob.NewDecoder(reader)
	if err := decoder.Decode(&props); err != nil {
		return props, fmt.Errorf("failed to gob-decode body properties: %w", err)
	}
	return props, nil
}

func (s *store) CreateRemoteHook(ctx context.Context, hook *RemoteHook) error {
	now := time.Now().UTC()
	hook.CreatedAt = now
	hook.UpdatedAt = now
	if hook.ID == "" {
		hook.ID = uuid.NewString()
	}

	headersJSON, err := json.Marshal(hook.Headers)
	if err != nil {
		return fmt.Errorf("failed to marshal hook headers: %w", err)
	}

	// Use gob encoding for body properties
	bodyPropsBytes, err := encodeProperties(hook.Properties)
	if err != nil {
		return err
	}

	_, err = s.Exec.ExecContext(ctx, `
        INSERT INTO remote_hooks
        (id, name, endpoint_url, timeout_ms, headers, properties, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hook.ID,
		hook.Name,
		hook.EndpointURL,
		hook.TimeoutMs,
		headersJSON,
		bodyPropsBytes,
		hook.CreatedAt,
		hook.UpdatedAt,
	)
	return err
}

func (s *store) GetRemoteHook(ctx context.Context, id string) (*RemoteHook, error) {
	var hook RemoteHook
	var headersJSON, bodyPropsBytes []byte

	err := s.Exec.QueryRowContext(ctx, `
        SELECT id, name, endpoint_url, timeout_ms, headers, properties, created_at, updated_at
        FROM remote_hooks
        WHERE id = $1`, id).Scan(
		&hook.ID,
		&hook.Name,
		&hook.EndpointURL,
		&hook.TimeoutMs,
		&headersJSON,
		&bodyPropsBytes,
		&hook.CreatedAt,
		&hook.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}

	if err := json.Unmarshal(headersJSON, &hook.Headers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal hook headers: %w", err)
	}

	// Use gob decoding for body properties
	props, err := decodeProperties(bodyPropsBytes)
	if err != nil {
		return nil, err
	}
	hook.Properties = props

	return &hook, nil
}

func (s *store) GetRemoteHookByName(ctx context.Context, name string) (*RemoteHook, error) {
	var hook RemoteHook
	var headersJSON, bodyPropsBytes []byte

	err := s.Exec.QueryRowContext(ctx, `
        SELECT id, name, endpoint_url,  timeout_ms, headers, properties, created_at, updated_at
        FROM remote_hooks
        WHERE name = $1`, name).Scan(
		&hook.ID,
		&hook.Name,
		&hook.EndpointURL,
		&hook.TimeoutMs,
		&headersJSON,
		&bodyPropsBytes,
		&hook.CreatedAt,
		&hook.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}

	if err := json.Unmarshal(headersJSON, &hook.Headers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal hook headers: %w", err)
	}

	// Use gob decoding for body properties
	props, err := decodeProperties(bodyPropsBytes)
	if err != nil {
		return nil, err
	}
	hook.Properties = props

	return &hook, nil
}

func (s *store) UpdateRemoteHook(ctx context.Context, hook *RemoteHook) error {
	hook.UpdatedAt = time.Now().UTC()

	headersJSON, err := json.Marshal(hook.Headers)
	if err != nil {
		return fmt.Errorf("failed to marshal hook headers for update: %w", err)
	}

	// Use gob encoding for body properties
	bodyPropsBytes, err := encodeProperties(hook.Properties)
	if err != nil {
		return err
	}

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE remote_hooks
		SET name = $2, endpoint_url = $3,  timeout_ms = $4, headers = $5, properties = $6, updated_at = $7
		WHERE id = $1`,
		hook.ID,
		hook.Name,
		hook.EndpointURL,
		hook.TimeoutMs,
		headersJSON,
		bodyPropsBytes,
		hook.UpdatedAt,
	)

	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) ListRemoteHooks(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*RemoteHook, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}

	rows, err := s.Exec.QueryContext(ctx, `
        SELECT id, name, endpoint_url, timeout_ms, headers, properties, created_at, updated_at
        FROM remote_hooks
        WHERE created_at < $1
        ORDER BY created_at DESC, id DESC
        LIMIT $2;
    `, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query remote hooks: %w", err)
	}
	defer rows.Close()

	hooks := []*RemoteHook{}
	for rows.Next() {
		var hook RemoteHook
		var headersJSON, bodyPropsBytes []byte
		if err := rows.Scan(
			&hook.ID,
			&hook.Name,
			&hook.EndpointURL,
			&hook.TimeoutMs,
			&headersJSON,
			&bodyPropsBytes,
			&hook.CreatedAt,
			&hook.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan remote hook: %w", err)
		}

		if err := json.Unmarshal(headersJSON, &hook.Headers); err != nil {
			return nil, fmt.Errorf("failed to unmarshal hook headers from list: %w", err)
		}

		// Use gob decoding for body properties
		props, err := decodeProperties(bodyPropsBytes)
		if err != nil {
			return nil, err
		}
		hook.Properties = props

		hooks = append(hooks, &hook)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return hooks, nil
}

func (s *store) DeleteRemoteHook(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM remote_hooks
		WHERE id = $1`, id)

	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) EstimateRemoteHookCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "remote_hooks")
}
