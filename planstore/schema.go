package planstore

import (
	"context"
	"strings"

	"github.com/contenox/contenox/libdbexec"
)

// InitSchema creates the plans and plan_steps tables if they do not exist.
func InitSchema(ctx context.Context, exec libdbexec.Exec) error {
	_, err := exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS plans (
			id         VARCHAR(255) PRIMARY KEY,
			name       VARCHAR(255) NOT NULL UNIQUE,
			goal       TEXT         NOT NULL,
			status     VARCHAR(50)  NOT NULL DEFAULT 'active',
			session_id VARCHAR(255),
			compiled_chain_json          TEXT,
			compiled_chain_id            VARCHAR(255),
			compile_executor_chain_id    VARCHAR(255),
			created_at TIMESTAMP    NOT NULL,
			updated_at TIMESTAMP    NOT NULL
		);

		CREATE TABLE IF NOT EXISTS plan_steps (
			id               VARCHAR(255) PRIMARY KEY,
			plan_id          VARCHAR(255) NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
			ordinal          INT          NOT NULL,
			description      TEXT         NOT NULL,
			status           VARCHAR(50)  NOT NULL DEFAULT 'pending',
			execution_result TEXT         NOT NULL DEFAULT '',
			executed_at      TIMESTAMP,
			UNIQUE (plan_id, ordinal)
		);

		CREATE INDEX IF NOT EXISTS idx_plan_steps_plan_id ON plan_steps(plan_id);
	`)
	if err != nil {
		return err
	}
	return migratePlansCompiledColumns(ctx, exec)
}

// migratePlansCompiledColumns adds compile columns to existing databases created before they existed.
func migratePlansCompiledColumns(ctx context.Context, exec libdbexec.Exec) error {
	stmts := []string{
		`ALTER TABLE plans ADD COLUMN compiled_chain_json TEXT`,
		`ALTER TABLE plans ADD COLUMN compiled_chain_id VARCHAR(255)`,
		`ALTER TABLE plans ADD COLUMN compile_executor_chain_id VARCHAR(255)`,
	}
	for _, q := range stmts {
		_, err := exec.ExecContext(ctx, q)
		if err != nil && !isDuplicateColumnError(err) {
			return err
		}
	}
	return nil
}

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists")
}
