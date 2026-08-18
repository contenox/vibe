package contenoxcli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/substrate"
	"github.com/stretchr/testify/require"
)

func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

func TestUnit_dbPathIsExplicit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	def, err := globalDBPath()
	require.NoError(t, err)

	require.False(t, dbPathIsExplicit(def))
	require.True(t, dbPathIsExplicit(filepath.Join(t.TempDir(), "other.db")))
}

func TestUnit_OpenDBAt_ExplicitPathWithPostgresNamesBoth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetSubstrateEnv(t)
	t.Setenv(substrate.PostgresURLEnv, "postgres://contenox@db:5432/contenox")
	t.Setenv(substrate.NATSURLEnv, "nats://bus:4222")
	t.Setenv(substrate.ValkeyURLEnv, "valkey://cache:6379")

	explicitPath := filepath.Join(t.TempDir(), "explicit.db")
	_, err := OpenDBAt(context.Background(), explicitPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), substrate.PostgresURLEnv)
	require.Contains(t, err.Error(), explicitPath)
	require.NoFileExists(t, explicitPath)
}

func TestUnit_openOptionalDB_NATSOnlyLeavesALocalStoreFailureNonFatal(t *testing.T) {
	unsetSubstrateEnv(t)
	t.Setenv(substrate.NATSURLEnv, "nats://bus:4222")

	dir := t.TempDir()
	badPath := filepath.Join(dir, "not-a-file")
	require.NoError(t, os.MkdirAll(badPath, 0o755))

	db, err := openOptionalDB(context.Background(), badPath)
	require.NoError(t, err)
	require.Nil(t, db)
}

func TestUnit_openOptionalDB_PostgresConfiguredMakesTheStoresOwnFailureFatal(t *testing.T) {
	if testing.Short() {
		t.Skip("dials a port that must be closed")
	}
	unsetSubstrateEnv(t)
	t.Setenv(substrate.PostgresURLEnv, "postgres://contenox:topsecret@"+closedLoopbackAddr(t)+"/contenox?sslmode=disable")
	t.Setenv(substrate.NATSURLEnv, "nats://bus:4222")
	t.Setenv(substrate.ValkeyURLEnv, "valkey://cache:6379")

	db, err := openOptionalDB(context.Background(), filepath.Join(t.TempDir(), "local.db"))
	require.Error(t, err)
	require.Nil(t, db)
}
