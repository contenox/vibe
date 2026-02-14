package functionstore

import (
	"context"

	"github.com/contenox/vibe/libdbexec"
)

// InitSchema creates the main table
func InitSchema(ctx context.Context, exec libdbexec.Exec) error {
	_, err := exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS functions (
		    name TEXT PRIMARY KEY,
		    description TEXT,
		    script_type TEXT NOT NULL,
		    script TEXT NOT NULL,
		    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
		    updated_at TIMESTAMP WITH TIME ZONE NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS event_triggers (
		    name TEXT PRIMARY KEY,
		    description TEXT,
		    listen_for_type TEXT NOT NULL,
		    trigger_type TEXT NOT NULL,
		    function_name TEXT NOT NULL REFERENCES functions(name) ON DELETE CASCADE,
		    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
		    updated_at TIMESTAMP WITH TIME ZONE NOT NULL
		);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_functions_created_at ON functions(created_at);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_event_triggers_created_at ON event_triggers(created_at);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_event_triggers_listen_for_type ON event_triggers(listen_for_type);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_event_triggers_function_name ON event_triggers(function_name);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION estimate_row_count(table_name TEXT)
		RETURNS BIGINT AS $$
		DECLARE
		    result BIGINT;
		BEGIN
		    SELECT reltuples::BIGINT
		    INTO result
		    FROM pg_class
		    WHERE relname = table_name;

		    RETURN COALESCE(result, 0);
		END;
		$$ LANGUAGE plpgsql STABLE;
	`)
	if err != nil {
		return err
	}

	return nil
}
