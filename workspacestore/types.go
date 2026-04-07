package workspacestore

import (
	"context"
	"time"
)

// Workspace maps to the workspaces table.
type Workspace struct {
	ID        string    `json:"id"`
	Principal string    `json:"principal"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Shell     string    `json:"shell,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store is data access for user workspaces.
type Store interface {
	Insert(ctx context.Context, w *Workspace) error
	GetByID(ctx context.Context, id string) (*Workspace, error)
	GetByIDAndPrincipal(ctx context.Context, id, principal string) (*Workspace, error)
	ListByPrincipal(ctx context.Context, principal string, createdAtCursor *time.Time, limit int) ([]*Workspace, error)
	Update(ctx context.Context, w *Workspace) error
	DeleteByIDAndPrincipal(ctx context.Context, id, principal string) error
}
