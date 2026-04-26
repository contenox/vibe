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

	libdb "github.com/contenox/contenox/libdbexec"
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

// orEmptyStringMap returns m if non-nil, otherwise an empty map.
func orEmptyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (s *store) CreateRemoteTools(ctx context.Context, tools *RemoteTools) error {
	now := time.Now().UTC()
	tools.CreatedAt = now
	tools.UpdatedAt = now
	if tools.ID == "" {
		tools.ID = uuid.NewString()
	}

	headersJSON, err := json.Marshal(tools.Headers)
	if err != nil {
		return fmt.Errorf("failed to marshal tools headers: %w", err)
	}

	injectJSON, err := json.Marshal(orEmptyStringMap(tools.InjectParams))
	if err != nil {
		return fmt.Errorf("failed to marshal tools inject params: %w", err)
	}

	// Use gob encoding for body properties
	bodyPropsBytes, err := encodeProperties(tools.Properties)
	if err != nil {
		return err
	}

	_, err = s.Exec.ExecContext(ctx, `
        INSERT INTO remote_tools
        (id, name, endpoint_url, spec_url, timeout_ms, headers, properties, inject_params_json, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		tools.ID,
		tools.Name,
		tools.EndpointURL,
		tools.SpecURL,
		tools.TimeoutMs,
		headersJSON,
		bodyPropsBytes,
		string(injectJSON),
		tools.CreatedAt,
		tools.UpdatedAt,
	)
	return err
}

func (s *store) GetRemoteTools(ctx context.Context, id string) (*RemoteTools, error) {
	var tools RemoteTools
	var headersJSON, bodyPropsBytes []byte
	var injectJSON string

	err := s.Exec.QueryRowContext(ctx, `
        SELECT id, name, endpoint_url, spec_url, timeout_ms, headers, properties, inject_params_json, created_at, updated_at
        FROM remote_tools
        WHERE id = $1`, id).Scan(
		&tools.ID,
		&tools.Name,
		&tools.EndpointURL,
		&tools.SpecURL,
		&tools.TimeoutMs,
		&headersJSON,
		&bodyPropsBytes,
		&injectJSON,
		&tools.CreatedAt,
		&tools.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}

	if err := json.Unmarshal(headersJSON, &tools.Headers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tools headers: %w", err)
	}

	// Use gob decoding for body properties
	props, err := decodeProperties(bodyPropsBytes)
	if err != nil {
		return nil, err
	}
	tools.Properties = props

	if injectJSON != "" && injectJSON != "{}" && injectJSON != "null" {
		if err := json.Unmarshal([]byte(injectJSON), &tools.InjectParams); err != nil {
			tools.InjectParams = nil
		}
	}

	return &tools, nil
}

func (s *store) GetRemoteToolsByName(ctx context.Context, name string) (*RemoteTools, error) {
	var tools RemoteTools
	var headersJSON, bodyPropsBytes []byte
	var injectJSON string

	err := s.Exec.QueryRowContext(ctx, `
        SELECT id, name, endpoint_url, spec_url, timeout_ms, headers, properties, inject_params_json, created_at, updated_at
        FROM remote_tools
        WHERE name = $1`, name).Scan(
		&tools.ID,
		&tools.Name,
		&tools.EndpointURL,
		&tools.SpecURL,
		&tools.TimeoutMs,
		&headersJSON,
		&bodyPropsBytes,
		&injectJSON,
		&tools.CreatedAt,
		&tools.UpdatedAt,
	)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}

	if err := json.Unmarshal(headersJSON, &tools.Headers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tools headers: %w", err)
	}

	// Use gob decoding for body properties
	props, err := decodeProperties(bodyPropsBytes)
	if err != nil {
		return nil, err
	}
	tools.Properties = props

	if injectJSON != "" && injectJSON != "{}" && injectJSON != "null" {
		if err := json.Unmarshal([]byte(injectJSON), &tools.InjectParams); err != nil {
			tools.InjectParams = nil
		}
	}

	return &tools, nil
}

func (s *store) UpdateRemoteTools(ctx context.Context, tools *RemoteTools) error {
	tools.UpdatedAt = time.Now().UTC()

	headersJSON, err := json.Marshal(tools.Headers)
	if err != nil {
		return fmt.Errorf("failed to marshal tools headers for update: %w", err)
	}

	injectJSON, err := json.Marshal(orEmptyStringMap(tools.InjectParams))
	if err != nil {
		return fmt.Errorf("failed to marshal tools inject params for update: %w", err)
	}

	// Use gob encoding for body properties
	bodyPropsBytes, err := encodeProperties(tools.Properties)
	if err != nil {
		return err
	}

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE remote_tools
		SET name = $2, endpoint_url = $3, spec_url = $4, timeout_ms = $5, headers = $6, properties = $7, inject_params_json = $8, updated_at = $9
		WHERE id = $1`,
		tools.ID,
		tools.Name,
		tools.EndpointURL,
		tools.SpecURL,
		tools.TimeoutMs,
		headersJSON,
		bodyPropsBytes,
		string(injectJSON),
		tools.UpdatedAt,
	)

	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) ListRemoteTools(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*RemoteTools, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}

	rows, err := s.Exec.QueryContext(ctx, `
        SELECT id, name, endpoint_url, spec_url, timeout_ms, headers, properties, inject_params_json, created_at, updated_at
        FROM remote_tools
        WHERE created_at < $1
        ORDER BY created_at DESC, id DESC
        LIMIT $2;
    `, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query remote tools: %w", err)
	}
	defer rows.Close()

	allTools := []*RemoteTools{}
	for rows.Next() {
		var tools RemoteTools
		var headersJSON, bodyPropsBytes []byte
		var injectJSON string
		if err := rows.Scan(
			&tools.ID,
			&tools.Name,
			&tools.EndpointURL,
			&tools.SpecURL,
			&tools.TimeoutMs,
			&headersJSON,
			&bodyPropsBytes,
			&injectJSON,
			&tools.CreatedAt,
			&tools.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan remote tools: %w", err)
		}

		if err := json.Unmarshal(headersJSON, &tools.Headers); err != nil {
			return nil, fmt.Errorf("failed to unmarshal tools headers from list: %w", err)
		}

		// Use gob decoding for body properties
		props, err := decodeProperties(bodyPropsBytes)
		if err != nil {
			return nil, err
		}
		tools.Properties = props

		if injectJSON != "" && injectJSON != "{}" && injectJSON != "null" {
			if err := json.Unmarshal([]byte(injectJSON), &tools.InjectParams); err != nil {
				tools.InjectParams = nil
			}
		}

		allTools = append(allTools, &tools)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return allTools, nil
}

func (s *store) DeleteRemoteTools(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `
		DELETE FROM remote_tools
		WHERE id = $1`, id)

	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) EstimateRemoteToolsCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "remote_tools")
}
