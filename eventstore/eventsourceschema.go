package eventstore

import (
	"context"

	"github.com/contenox/vibe/libdbexec"
)

// InitSchema creates the main events table, raw_events table, and initial partitions
func InitSchema(ctx context.Context, exec libdbexec.Exec) error {
	_, err := exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS events (
			id TEXT NOT NULL,
			nid BIGSERIAL NOT NULL,
			partition_key TEXT NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL,
			event_type TEXT NOT NULL,
			event_source TEXT NOT NULL,
			aggregate_id TEXT NOT NULL,
			aggregate_type TEXT NOT NULL,
			version INTEGER NOT NULL,
			data JSONB NOT NULL,
			metadata JSONB,
			PRIMARY KEY (id, event_type, event_source, partition_key)
		) PARTITION BY LIST (partition_key);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS raw_events (
			id TEXT NOT NULL,
			nid BIGSERIAL NOT NULL,
			partition_key TEXT NOT NULL,
			received_at TIMESTAMP WITH TIME ZONE NOT NULL,
			path TEXT NOT NULL,
			headers BYTEA,
			payload BYTEA NOT NULL,
			processed BOOLEAN NOT NULL DEFAULT FALSE,
			error_message TEXT,
			PRIMARY KEY (nid, partition_key)
		) PARTITION BY LIST (partition_key);
	`)
	if err != nil {
		return err
	}

	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_events_partition_key ON events (partition_key);
	`); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_events_created_at_brin ON events USING BRIN (created_at);
	`); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_events_event_type ON events (event_type);
	`); err != nil {
		return err
	}

	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_raw_events_partition_key ON raw_events (partition_key);
	`); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_raw_events_received_at_brin ON raw_events USING BRIN (received_at);
	`); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_raw_events_path ON raw_events (path);
	`); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_raw_events_unprocessed
		ON raw_events (received_at) WHERE processed = FALSE;
	`); err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS event_mappings (
			path TEXT PRIMARY KEY,
			event_type TEXT NOT NULL,
			event_source TEXT NOT NULL,
			aggregate_type TEXT NOT NULL,
			aggregate_id_field TEXT,
			aggregate_type_field TEXT,
			event_type_field TEXT,
			event_source_field TEXT,
			event_id_field TEXT,
			version INTEGER NOT NULL DEFAULT 1,
			metadata_mapping JSONB NOT NULL DEFAULT '{}'
		);
	`)
	if err != nil {
		return err
	}

	return nil
}
