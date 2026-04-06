package terminalstore

import (
	"context"

	"github.com/contenox/contenox/libdbexec"
)

// InitSchema creates terminal_sessions if missing (matches runtimetypes/schema.sql).
func InitSchema(ctx context.Context, exec libdbexec.Exec) error {
	_, err := exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS terminal_sessions (
			id VARCHAR(255) PRIMARY KEY,
			principal VARCHAR(512) NOT NULL,
			cwd TEXT NOT NULL,
			shell VARCHAR(512) NOT NULL,
			cols INT NOT NULL,
			rows INT NOT NULL,
			status VARCHAR(50) NOT NULL DEFAULT 'active',
			node_instance_id VARCHAR(255) NOT NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_terminal_sessions_principal_created ON terminal_sessions (principal, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_terminal_sessions_node ON terminal_sessions (node_instance_id);
	`)
	return err
}
