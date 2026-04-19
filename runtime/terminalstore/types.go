package terminalstore

import (
	"context"
	"time"
)

// SessionStatus is persisted terminal session lifecycle.
type SessionStatus string

const (
	SessionStatusActive SessionStatus = "active"
	SessionStatusClosed SessionStatus = "closed"
)

// Session maps to the terminal_sessions table.
type Session struct {
	ID             string        `json:"id"`
	Principal      string        `json:"principal"`
	CWD            string        `json:"cwd"`
	Shell          string        `json:"shell"`
	Cols           int           `json:"cols"`
	Rows           int           `json:"rows"`
	Status         SessionStatus `json:"status"`
	NodeInstanceID string        `json:"nodeInstanceId"`
	WorkspaceID    string        `json:"workspaceId,omitempty"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
}

// Store is data access for interactive terminal session metadata.
type Store interface {
	Insert(ctx context.Context, s *Session) error
	GetByID(ctx context.Context, id string) (*Session, error)
	GetByIDAndPrincipal(ctx context.Context, id, principal string) (*Session, error)
	ListByPrincipal(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*Session, error)
	UpdateGeometry(ctx context.Context, id string, cols, rows int) error
	Delete(ctx context.Context, id string) error
	DeleteByNodeInstanceID(ctx context.Context, nodeInstanceID string) error
}
