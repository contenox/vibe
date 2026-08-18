package substrate

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libbus"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

const (
	requireNATSEnv   = "LIBBUS_REQUIRE_NATS"
	requireValkeyEnv = "LIBKVSTORE_REQUIRE_VALKEY"
)

func requireSubstrateBackend(t *testing.T, requiredEnv, backend string) {
	t.Helper()
	if os.Getenv(requiredEnv) != "" {
		return
	}
	if testing.Short() {
		t.Skipf("%s substrate wiring NOT exercised: needs a container. Set %s=1 to turn this skip into a failure.", backend, requiredEnv)
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
}

func TestUnit_OpenDB_PostgresSettingSelectsPostgresAndNeverTheFile(t *testing.T) {
	requireSubstrateBackend(t, runtimetypes.TestPostgresRequiredEnv, "Postgres")
	ctx := context.Background()

	dsn, _, cleanup, err := libdb.SetupLocalInstance(ctx, "contenox_substrate_test", "contenox", "contenox")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	t.Setenv(PostgresURLEnv, dsn)
	t.Setenv(NATSURLEnv, "nats://"+closedLoopbackAddr(t))
	t.Setenv(ValkeyURLEnv, "valkey://"+closedLoopbackAddr(t))

	sqlitePath := filepath.Join(t.TempDir(), "never-created", "local.db")
	db, err := OpenDB(ctx, sqlitePath, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	require.NoFileExists(t, sqlitePath)

	backends, err := runtimetypes.New(db.WithoutTransaction()).ListBackends(ctx, nil, 10)
	require.NoError(t, err)
	require.Empty(t, backends)

	var estimate int64
	require.NoError(t, db.WithoutTransaction().
		QueryRowContext(ctx, `SELECT estimate_row_count($1)`, "agents").Scan(&estimate))
}

func TestUnit_OpenBus_NATSSettingReachesTheServerItNames(t *testing.T) {
	requireSubstrateBackend(t, requireNATSEnv, "NATS")
	ctx := context.Background()

	natsURL, _, cleanup, err := libbus.SetupNatsInstance(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	clearSubstrateEnv(t)
	t.Setenv(NATSURLEnv, natsURL)

	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "local.db"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	bus, err := OpenBus(ctx, db.WithoutTransaction())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bus.Close()) })

	received := make(chan []byte, 1)
	subscription, err := bus.Stream(ctx, "substrate.test", received)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, subscription.Unsubscribe()) })

	require.NoError(t, bus.Publish(ctx, "substrate.test", []byte("ping")))
	select {
	case msg := <-received:
		require.Equal(t, []byte("ping"), msg)
	case <-time.After(10 * time.Second):
		t.Fatal("the NATS server the setting names accepted the publish but never delivered it")
	}

	var count int
	require.NoError(t, db.WithoutTransaction().
		QueryRowContext(ctx, `SELECT COUNT(*) FROM bus_events`).Scan(&count))
	require.Zero(t, count, "a NATS selection must never fall back to the SQLite bus tables")
}

func TestUnit_OpenKV_ValkeySettingReachesTheServerItNames(t *testing.T) {
	requireSubstrateBackend(t, requireValkeyEnv, "Valkey")
	ctx := context.Background()

	conn, _, cleanup, err := libkvstore.SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	target, err := url.Parse(conn)
	require.NoError(t, err)

	clearSubstrateEnv(t)
	t.Setenv(ValkeyURLEnv, "valkey://"+target.Host+"?namespace=substrate-test")

	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "local.db"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	kv, releaseKV, err := OpenKV(ctx, db)
	require.NoError(t, err)
	t.Cleanup(releaseKV)

	exec, err := kv.Executor(ctx)
	require.NoError(t, err)
	require.NoError(t, exec.Set(ctx, "substrate:test", []byte(`{"ok":true}`)))

	value, err := exec.Get(ctx, "substrate:test")
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(value))

	var count int
	require.NoError(t, db.WithoutTransaction().
		QueryRowContext(ctx, `SELECT COUNT(*) FROM kv_store`).Scan(&count))
	require.Zero(t, count, "a Valkey selection must never fall back to the SQLite kv table")
}

func TestUnit_OpenBusAndOpenKV_RefuseAnUnreachableServerByName(t *testing.T) {
	clearSubstrateEnv(t)
	ctx := context.Background()

	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "local.db"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	t.Setenv(NATSURLEnv, "nats://"+closedLoopbackAddr(t))
	_, err = OpenBus(ctx, db.WithoutTransaction())
	require.Error(t, err)
	require.Contains(t, err.Error(), NATSURLEnv)

	t.Setenv(NATSURLEnv, "")
	t.Setenv(ValkeyURLEnv, "valkey://"+closedLoopbackAddr(t))
	_, _, err = OpenKV(ctx, db)
	require.Error(t, err)
	require.Contains(t, err.Error(), ValkeyURLEnv)
}
