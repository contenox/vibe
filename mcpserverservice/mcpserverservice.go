// Package mcpserverservice provides CRUD operations for MCP server configurations.
// MCP server configs are persisted in the shared database and consumed by
// PersistentRepo at hook-execution time via transient connections (distributed-safe).
package mcpserverservice

import (
	"context"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/localhooks"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/google/uuid"
)

// Service exposes CRUD operations for persisted MCP server configurations.
type Service interface {
	Create(ctx context.Context, srv *runtimetypes.MCPServer) error
	Get(ctx context.Context, id string) (*runtimetypes.MCPServer, error)
	GetByName(ctx context.Context, name string) (*runtimetypes.MCPServer, error)
	Update(ctx context.Context, srv *runtimetypes.MCPServer) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.MCPServer, error)
	AuthenticateOAuth(ctx context.Context, name string, oauthCfg *localhooks.MCPOAuthConfig) error
	StartOAuth(ctx context.Context, id, redirectBase string) (*OAuthStartResult, error)
	CompleteOAuth(ctx context.Context, req OAuthCallbackRequest) (*OAuthCallbackResult, error)
}

type service struct {
	db        libdb.DBManager
	uiBaseURL string
}

// New creates a new MCP server service backed by the given database manager.
func New(db libdb.DBManager, opts ...Option) Service {
	s := &service{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type Option func(*service)

func WithUIBaseURL(uiBaseURL string) Option {
	return func(s *service) {
		s.uiBaseURL = uiBaseURL
	}
}

func (s *service) store() runtimetypes.Store {
	return runtimetypes.New(s.db.WithoutTransaction())
}

func (s *service) Create(ctx context.Context, srv *runtimetypes.MCPServer) error {
	if err := validate(srv); err != nil {
		return err
	}
	if srv.ID == "" {
		srv.ID = uuid.NewString()
	}
	return s.store().CreateMCPServer(ctx, srv)
}

func (s *service) Get(ctx context.Context, id string) (*runtimetypes.MCPServer, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	return s.store().GetMCPServer(ctx, id)
}

func (s *service) GetByName(ctx context.Context, name string) (*runtimetypes.MCPServer, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	return s.store().GetMCPServerByName(ctx, name)
}

func (s *service) Update(ctx context.Context, srv *runtimetypes.MCPServer) error {
	if srv.ID == "" {
		return fmt.Errorf("id is required for update")
	}
	if err := validate(srv); err != nil {
		return err
	}
	return s.store().UpdateMCPServer(ctx, srv)
}

func (s *service) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	return s.store().DeleteMCPServer(ctx, id)
}

func (s *service) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.MCPServer, error) {
	return s.store().ListMCPServers(ctx, createdAtCursor, limit)
}

func validate(srv *runtimetypes.MCPServer) error {
	if srv.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch srv.Transport {
	case "stdio":
		if srv.Command == "" {
			return fmt.Errorf("command is required for stdio transport")
		}
	case "sse", "http":
		if srv.URL == "" {
			return fmt.Errorf("url is required for %s transport", srv.Transport)
		}
	case "":
		return fmt.Errorf("transport is required (stdio, sse, or http)")
	default:
		return fmt.Errorf("unknown transport %q: must be stdio, sse, or http", srv.Transport)
	}
	return nil
}
