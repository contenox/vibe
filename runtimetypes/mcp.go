package runtimetypes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/google/uuid"
)

type contextKey string

const SessionIDContextKey contextKey = "contenox_session_id"

// orEmptyMap returns m if non-nil, otherwise an empty map.
func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// MCPServer represents a persisted MCP server configuration.
type MCPServer struct {
	ID                    string            `json:"id" example:"a1b2c3d4-e5f6-7890-abcd-ef1234567890"`
	Name                  string            `json:"name" example:"filesystem"`
	Transport             string            `json:"transport" example:"sse"` // sse | http | stdio
	Command               string            `json:"command,omitempty" example:"npx"`
	Args                  []string          `json:"args,omitempty" example:"['-y','@modelcontextprotocol/server-filesystem','/tmp']" openapi_include_type:"string"`
	URL                   string            `json:"url,omitempty" example:"http://mcp-fs:8080/sse"`
	AuthType              string            `json:"authType,omitempty" example:"bearer"`         // "" | "bearer" | "oauth"
	AuthToken             string            `json:"authToken,omitempty"`                         // literal token (avoid in prod)
	AuthEnvKey            string            `json:"authEnvKey,omitempty" example:"MCP_FS_TOKEN"` // env var name
	ConnectTimeoutSeconds int               `json:"connectTimeoutSeconds" example:"30"`
	Headers               map[string]string `json:"headers,omitempty"`      // additional HTTP headers for SSE/HTTP transports
	InjectParams          map[string]string `json:"injectParams,omitempty"` // injected as tool call args, hidden from model schema
	CreatedAt             time.Time         `json:"createdAt" example:"2024-01-15T10:00:00Z"`
	UpdatedAt             time.Time         `json:"updatedAt" example:"2024-01-15T10:00:00Z"`
}

// MCPTool is a minimal tool descriptor returned by mcpworker list-tools.
// It avoids importing the MCP SDK in packages that only need the tool metadata.
type MCPTool struct {
	// Name is the tool identifier as advertised by the MCP server.
	Name string `json:"name" example:"read_file"`
	// Description is a human-readable description of the tool.
	Description string `json:"description,omitempty" example:"Read a file from the filesystem"`
	// InputSchema is the raw JSON schema for the tool's input parameters.
	// Preserved as json.RawMessage so it survives NATS serialization unchanged.
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

func (s *store) CreateMCPServer(ctx context.Context, srv *MCPServer) error {
	now := time.Now().UTC()
	srv.CreatedAt = now
	srv.UpdatedAt = now
	if srv.ID == "" {
		srv.ID = uuid.NewString()
	}

	argsJSON, err := json.Marshal(srv.Args)
	if err != nil {
		return fmt.Errorf("mcp: marshal args: %w", err)
	}
	if srv.Args == nil {
		argsJSON = []byte("[]")
	}

	headersJSON, _ := json.Marshal(orEmptyMap(srv.Headers))
	injectJSON, _ := json.Marshal(orEmptyMap(srv.InjectParams))

	timeout := srv.ConnectTimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}

	_, err = s.Exec.ExecContext(ctx, `
		INSERT INTO mcp_servers
		(id, name, transport, command, args_json, url, auth_type, auth_token, auth_env_key, connect_timeout_seconds, headers_json, inject_params_json, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		srv.ID, srv.Name, srv.Transport, srv.Command, string(argsJSON),
		srv.URL, srv.AuthType, srv.AuthToken, srv.AuthEnvKey,
		timeout, string(headersJSON), string(injectJSON), srv.CreatedAt, srv.UpdatedAt,
	)
	return err
}

// UpsertMCPServerByName inserts or updates an MCP server keyed by name.
// If a server with the same name already exists, its fields are updated in place
// (preserving the original ID and created_at). If not, a new record is inserted.
// Works on both SQLite and Postgres via the ON CONFLICT clause.
func (s *store) UpsertMCPServerByName(ctx context.Context, srv *MCPServer) error {
	now := time.Now().UTC()
	if srv.ID == "" {
		srv.ID = uuid.NewString()
	}
	srv.CreatedAt = now
	srv.UpdatedAt = now

	argsJSON, err := json.Marshal(srv.Args)
	if err != nil {
		return fmt.Errorf("mcp: marshal args: %w", err)
	}
	if srv.Args == nil {
		argsJSON = []byte("[]")
	}
	timeout := srv.ConnectTimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}

	headersJSON, _ := json.Marshal(orEmptyMap(srv.Headers))
	injectJSON, _ := json.Marshal(orEmptyMap(srv.InjectParams))

	_, err = s.Exec.ExecContext(ctx, `
		INSERT INTO mcp_servers
		(id, name, transport, command, args_json, url, auth_type, auth_token, auth_env_key, connect_timeout_seconds, headers_json, inject_params_json, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT(name) DO UPDATE SET
			transport               = excluded.transport,
			command                 = excluded.command,
			args_json               = excluded.args_json,
			url                     = excluded.url,
			auth_type               = excluded.auth_type,
			auth_token              = excluded.auth_token,
			auth_env_key            = excluded.auth_env_key,
			connect_timeout_seconds = excluded.connect_timeout_seconds,
			headers_json            = excluded.headers_json,
			inject_params_json      = excluded.inject_params_json,
			updated_at              = excluded.updated_at`,
		srv.ID, srv.Name, srv.Transport, srv.Command, string(argsJSON),
		srv.URL, srv.AuthType, srv.AuthToken, srv.AuthEnvKey,
		timeout, string(headersJSON), string(injectJSON), srv.CreatedAt, srv.UpdatedAt,
	)
	return err
}

func (s *store) GetMCPServer(ctx context.Context, id string) (*MCPServer, error) {
	return s.scanMCPServer(ctx, `
		SELECT id, name, transport, command, args_json, url, auth_type, auth_token, auth_env_key, connect_timeout_seconds, headers_json, inject_params_json, created_at, updated_at
		FROM mcp_servers WHERE id = $1`, id)
}

func (s *store) GetMCPServerByName(ctx context.Context, name string) (*MCPServer, error) {
	return s.scanMCPServer(ctx, `
		SELECT id, name, transport, command, args_json, url, auth_type, auth_token, auth_env_key, connect_timeout_seconds, headers_json, inject_params_json, created_at, updated_at
		FROM mcp_servers WHERE name = $1`, name)
}

func (s *store) scanMCPServer(ctx context.Context, query string, arg any) (*MCPServer, error) {
	var srv MCPServer
	var argsJSON, headersJSON, injectJSON string
	err := s.Exec.QueryRowContext(ctx, query, arg).Scan(
		&srv.ID, &srv.Name, &srv.Transport, &srv.Command, &argsJSON,
		&srv.URL, &srv.AuthType, &srv.AuthToken, &srv.AuthEnvKey,
		&srv.ConnectTimeoutSeconds, &headersJSON, &injectJSON, &srv.CreatedAt, &srv.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, libdb.ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(argsJSON), &srv.Args); err != nil {
		srv.Args = nil
	}
	if err := json.Unmarshal([]byte(headersJSON), &srv.Headers); err != nil {
		srv.Headers = nil
	}
	if err := json.Unmarshal([]byte(injectJSON), &srv.InjectParams); err != nil {
		srv.InjectParams = nil
	}
	return &srv, nil
}

func (s *store) UpdateMCPServer(ctx context.Context, srv *MCPServer) error {
	srv.UpdatedAt = time.Now().UTC()
	argsJSON, err := json.Marshal(srv.Args)
	if err != nil {
		return fmt.Errorf("mcp: marshal args: %w", err)
	}
	if srv.Args == nil {
		argsJSON = []byte("[]")
	}

	headersJSON, _ := json.Marshal(orEmptyMap(srv.Headers))
	injectJSON, _ := json.Marshal(orEmptyMap(srv.InjectParams))

	result, err := s.Exec.ExecContext(ctx, `
		UPDATE mcp_servers
		SET name=$2, transport=$3, command=$4, args_json=$5, url=$6,
		    auth_type=$7, auth_token=$8, auth_env_key=$9,
		    connect_timeout_seconds=$10, headers_json=$11, inject_params_json=$12, updated_at=$13
		WHERE id=$1`,
		srv.ID, srv.Name, srv.Transport, srv.Command, string(argsJSON),
		srv.URL, srv.AuthType, srv.AuthToken, srv.AuthEnvKey,
		srv.ConnectTimeoutSeconds, string(headersJSON), string(injectJSON), srv.UpdatedAt,
	)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) DeleteMCPServer(ctx context.Context, id string) error {
	result, err := s.Exec.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return checkRowsAffected(result)
}

func (s *store) ListMCPServers(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*MCPServer, error) {
	cursor := time.Now().UTC()
	if createdAtCursor != nil {
		cursor = *createdAtCursor
	}
	if limit > MAXLIMIT {
		return nil, ErrLimitParamExceeded
	}

	rows, err := s.Exec.QueryContext(ctx, `
		SELECT id, name, transport, command, args_json, url, auth_type, auth_token, auth_env_key, connect_timeout_seconds, headers_json, inject_params_json, created_at, updated_at
		FROM mcp_servers
		WHERE created_at < $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("mcp: list query: %w", err)
	}
	defer rows.Close()

	var out []*MCPServer
	for rows.Next() {
		var srv MCPServer
		var argsJSON, headersJSON, injectJSON string
		if err := rows.Scan(
			&srv.ID, &srv.Name, &srv.Transport, &srv.Command, &argsJSON,
			&srv.URL, &srv.AuthType, &srv.AuthToken, &srv.AuthEnvKey,
			&srv.ConnectTimeoutSeconds, &headersJSON, &injectJSON, &srv.CreatedAt, &srv.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("mcp: scan row: %w", err)
		}
		if err := json.Unmarshal([]byte(argsJSON), &srv.Args); err != nil {
			srv.Args = nil
		}
		if err := json.Unmarshal([]byte(headersJSON), &srv.Headers); err != nil {
			srv.Headers = nil
		}
		if err := json.Unmarshal([]byte(injectJSON), &srv.InjectParams); err != nil {
			srv.InjectParams = nil
		}
		out = append(out, &srv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcp: rows error: %w", err)
	}
	return out, nil
}

func (s *store) EstimateMCPServerCount(ctx context.Context) (int64, error) {
	return s.estimateCount(ctx, "mcp_servers")
}
