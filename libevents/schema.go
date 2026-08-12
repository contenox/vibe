package libevents

import (
	"context"
	"fmt"

	libdb "github.com/contenox/contenox/libdbexec"
)

// InitSchema creates the package's tables and indexes if absent; idempotent
// and safe to call against an already-compatible schema.
func InitSchema(ctx context.Context, exec libdb.Exec, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	stmts := []string{
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %[1]s (
    consumer   TEXT NOT NULL,
    %[2]s      TEXT NOT NULL,
    last_nid   BIGINT NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (consumer, %[2]s)
)`, cfg.table("cursors"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %[1]s (
    %[2]s        TEXT NOT NULL,
    trigger_name TEXT NOT NULL,
    nid          BIGINT NOT NULL,
    status       TEXT NOT NULL,
    error        TEXT,
    request_id   TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (%[2]s, trigger_name, nid)
)`, cfg.table("firings"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS idx_%[1]s_scope_nid
  ON %[1]s (%[2]s, nid DESC, trigger_name, status)`,
			cfg.table("firings"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %[1]s (
    id              TEXT NOT NULL,
    %[2]s           TEXT NOT NULL,
    kind            TEXT NOT NULL,
    target          TEXT NOT NULL,
    owner           TEXT NOT NULL,
    one_shot        BOOLEAN NOT NULL,
    context_filters TEXT NOT NULL,
    metadata        TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    PRIMARY KEY (%[2]s, id)
)`, cfg.table("listeners"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS idx_%[1]s_owner
  ON %[1]s (%[2]s, owner)`, cfg.table("listeners"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %[1]s (
    %[2]s       TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    listener_id TEXT NOT NULL,
    PRIMARY KEY (%[2]s, event_type, listener_id)
)`, cfg.table("listener_topics"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %[1]s (
    id            TEXT NOT NULL,
    %[2]s         TEXT NOT NULL,
    payload       TEXT NOT NULL,
    delayed_until TIMESTAMP NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    PRIMARY KEY (%[2]s, id)
)`, cfg.table("staging"), cfg.ScopeColumn),
		fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS idx_%[1]s_due
  ON %[1]s (delayed_until)`, cfg.table("staging")),
	}
	for _, stmt := range stmts {
		if _, err := exec.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("libevents: init schema: %w", err)
		}
	}
	return nil
}
