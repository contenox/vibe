package runtimetypes_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestUnit_HITLApprovals_AttributionMigratesExistingDatabase proves the UPGRADE
// path, not just the fresh-install one: a database whose hitl_approvals table
// predates the attribution columns gains them when the schema is re-applied, and
// the rows already in it survive. Every existing local install is this case, and
// a CREATE TABLE IF NOT EXISTS alone would silently leave them behind.
func TestUnit_HITLApprovals_AttributionMigratesExistingDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hitl_approvals_migration.db")

	// Build the PRE-attribution table by hand, with a row in it.
	raw, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `
		CREATE TABLE hitl_approvals (
		    id           VARCHAR(255) PRIMARY KEY,
		    tools_name   VARCHAR(255) NOT NULL,
		    tool_name    VARCHAR(255) NOT NULL,
		    args_summary TEXT NOT NULL DEFAULT '',
		    diff         TEXT,
		    policy_name  VARCHAR(255) NOT NULL DEFAULT '',
		    matched_rule INT,
		    on_timeout   VARCHAR(20) NOT NULL DEFAULT 'deny',
		    state        VARCHAR(20) NOT NULL DEFAULT 'pending',
		    resolution   TEXT,
		    created_at   TIMESTAMP NOT NULL,
		    expires_at   TIMESTAMP NOT NULL,
		    resolved_at  TIMESTAMP
		)`)
	require.NoError(t, err)
	legacyID := uuid.NewString()
	now := time.Now().UTC()
	_, err = raw.ExecContext(ctx, `
		INSERT INTO hitl_approvals (id, tools_name, tool_name, on_timeout, state, created_at, expires_at)
		VALUES (?, 'local_fs', 'write_file', 'deny', 'pending', ?, ?)`, legacyID, now, now.Add(time.Hour))
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Applying the current schema over it must migrate rather than fail.
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err, "re-applying the schema over a pre-attribution table must migrate it")
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())

	// The pre-existing row survives and reads back with empty attribution.
	legacy, err := store.GetHITLApproval(ctx, legacyID)
	require.NoError(t, err)
	require.Empty(t, legacy.InstanceID)
	require.Nil(t, legacy.MissionID)

	// And a new row can carry attribution through the migrated columns.
	missionID := uuid.NewString()
	fresh := &runtimetypes.HITLApproval{
		ID: uuid.NewString(), ToolsName: "local_fs", ToolName: "write_file",
		OnTimeout: "deny", State: runtimetypes.HITLApprovalPending,
		InstanceID: "instance-1", SessionID: "session-1", AgentName: "reviewer", MissionID: &missionID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(t, store.CreateHITLApproval(ctx, fresh))
	got, err := store.GetHITLApproval(ctx, fresh.ID)
	require.NoError(t, err)
	require.Equal(t, "instance-1", got.InstanceID)
	require.NotNil(t, got.MissionID)
}
