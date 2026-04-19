package planstore

import (
	"context"
	"strings"

	"github.com/contenox/contenox/libdbexec"
)

// InitSchema creates the plans and plan_steps tables if they do not exist.
//
// NOTE: In production the schema is applied by runtimetypes.SchemaSQLite /
// runtimetypes.Schema (loaded in runtime/contenoxcli/db_util.go and similar
// call sites). This function exists for unit tests that spin up an in-memory
// planstore without the full runtime. Keep the two schema shapes in sync
// whenever a column is added — see runtimetypes/schema_sqlite.sql for the
// canonical DDL.
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
			repo_context_json            TEXT,
			created_at TIMESTAMP    NOT NULL,
			updated_at TIMESTAMP    NOT NULL
		);

		CREATE TABLE IF NOT EXISTS plan_steps (
			id                    VARCHAR(255) PRIMARY KEY,
			plan_id               VARCHAR(255) NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
			ordinal               INT          NOT NULL,
			description           TEXT         NOT NULL,
			status                VARCHAR(50)  NOT NULL DEFAULT 'pending',
			execution_result      TEXT         NOT NULL DEFAULT '',
			executed_at           TIMESTAMP,
			summary               TEXT,
			chat_history_json     TEXT,
			summary_error         TEXT,
			last_failure_summary  TEXT,
			failure_class         VARCHAR(50),
			UNIQUE (plan_id, ordinal)
		);

		CREATE INDEX IF NOT EXISTS idx_plan_steps_plan_id ON plan_steps(plan_id);
	`)
	if err != nil {
		return err
	}
	if err := migratePlansCompiledColumns(ctx, exec); err != nil {
		return err
	}
	return migratePlanStepSummaryColumns(ctx, exec)
}

// migratePlansCompiledColumns adds compile columns to existing databases created before they existed.
func migratePlansCompiledColumns(ctx context.Context, exec libdbexec.Exec) error {
	stmts := []string{
		`ALTER TABLE plans ADD COLUMN compiled_chain_json TEXT`,
		`ALTER TABLE plans ADD COLUMN compiled_chain_id VARCHAR(255)`,
		`ALTER TABLE plans ADD COLUMN compile_executor_chain_id VARCHAR(255)`,
		`ALTER TABLE plans ADD COLUMN repo_context_json TEXT`,
	}
	for _, q := range stmts {
		_, err := exec.ExecContext(ctx, q)
		if err != nil && !isDuplicateColumnError(err) {
			return err
		}
	}
	return nil
}

// migratePlanStepSummaryColumns adds typed-handover columns (summary, chat history, summary error,
// last failure summary) to plan_steps on databases created before they existed.
func migratePlanStepSummaryColumns(ctx context.Context, exec libdbexec.Exec) error {
	stmts := []string{
		`ALTER TABLE plan_steps ADD COLUMN summary TEXT`,
		`ALTER TABLE plan_steps ADD COLUMN chat_history_json TEXT`,
		`ALTER TABLE plan_steps ADD COLUMN summary_error TEXT`,
		`ALTER TABLE plan_steps ADD COLUMN last_failure_summary TEXT`,
		`ALTER TABLE plan_steps ADD COLUMN failure_class VARCHAR(50)`,
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
