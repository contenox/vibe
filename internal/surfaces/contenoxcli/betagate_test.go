package contenoxcli

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func TestUnit_BetaEnabledGlobal_SQLiteOnlyStillSkipsWhenFileAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	require.False(t, betaEnabledGlobal())
}

// On a Postgres-only install the SQLite file betaEnabledGlobal used to stat is
// never created, so gating on os.Stat always answered "off" regardless of the
// opt-in row actually stored in Postgres.
func TestUnit_BetaEnabledGlobal_ReadsPostgresWhenSQLiteFileWasNeverCreated(t *testing.T) {
	if testing.Short() {
		t.Skipf("Postgres substrate wiring NOT exercised: needs a container. Set %s=1 to turn this skip into a failure.", runtimetypes.TestPostgresRequiredEnv)
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	dsn, _, cleanup, err := libdb.SetupLocalInstance(ctx, "contenox_betagate_test", "contenox", "contenox")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	t.Setenv(substrate.PostgresURLEnv, dsn)
	t.Setenv(substrate.NATSURLEnv, "nats://bus:4222")
	t.Setenv(substrate.ValkeyURLEnv, "valkey://cache:6379")

	dbPath, err := globalDBPath()
	require.NoError(t, err)
	require.NoFileExists(t, dbPath)

	seedCtx := libtracker.WithNewRequestID(ctx)
	db, err := OpenDBAt(seedCtx, dbPath)
	require.NoError(t, err)
	require.NoError(t, clikv.SetString(seedCtx, runtimetypes.New(db.WithoutTransaction()), optInBetaKey, "true"))
	require.NoError(t, db.Close())
	require.NoFileExists(t, dbPath, "opening the Postgres-backed store must not create the SQLite file")

	require.True(t, betaEnabledGlobal())
}
