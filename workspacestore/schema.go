package workspacestore

import (
	"context"

	"github.com/contenox/contenox/libdbexec"
)

// InitSchema creates workspaces if missing (matches runtimetypes schema).
func InitSchema(ctx context.Context, exec libdbexec.Exec) error {
	_, err := exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS workspaces (
			id VARCHAR(255) PRIMARY KEY,
			principal VARCHAR(512) NOT NULL,
			name VARCHAR(255) NOT NULL,
			path TEXT NOT NULL,
			shell VARCHAR(512),
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			UNIQUE (principal, name)
		);
		CREATE INDEX IF NOT EXISTS idx_workspaces_principal_created ON workspaces (principal, created_at DESC);
	`)
	return err
}
