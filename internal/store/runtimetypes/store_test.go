package runtimetypes_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	code := m.Run()
	runtimetypes.ShutdownTestBackends()
	os.Exit(code)
}

func TestUnit_Store_QueryingEmptyDB(t *testing.T) {
	ctx := context.TODO()
	dbManager, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "test.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	_ = runtimetypes.New(dbManager.WithoutTransaction())
	t.Cleanup(func() {
		require.NoError(t, dbManager.Close())
	})
}

func TestUnit_Store_PostgresEstimateIsUnknownUntilAnalyzed(t *testing.T) {
	if runtimetypes.TestBackendDefault() != runtimetypes.TestBackendPostgres {
		t.Skipf("estimate_row_count is a Postgres function; run with %s=%s",
			runtimetypes.TestBackendEnv, runtimetypes.TestBackendPostgres)
	}
	ctx, s, exec := runtimetypes.SetupStoreExec(t)

	for i := range 3 {
		require.NoError(t, s.CreateAgent(ctx, newExternalACPAgent(fmt.Sprintf("estimate-agent-%d", i))))
	}

	var raw int64
	require.NoError(t, exec.QueryRowContext(ctx, `SELECT estimate_row_count($1)`, "agents").Scan(&raw))
	require.Negative(t, raw, "pg_class.reltuples is -1 until the table is analyzed, so the raw estimate is unusable")

	_, err := exec.ExecContext(ctx, `ANALYZE agents`)
	require.NoError(t, err)
	require.NoError(t, exec.QueryRowContext(ctx, `SELECT estimate_row_count($1)`, "agents").Scan(&raw))
	require.EqualValues(t, 3, raw, "once analyzed, the estimate is the cheap answer the store prefers")

	count, err := s.EstimateAgentCount(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, count)
}
