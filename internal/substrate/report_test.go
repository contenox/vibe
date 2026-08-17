package substrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_Report_NothingSetNamesSQLiteForEverySubstrate(t *testing.T) {
	clearSubstrateEnv(t)
	path := filepath.Join(t.TempDir(), "local.db")

	statuses, err := Report(context.Background(), nil, path)
	require.NoError(t, err)
	require.False(t, AnyRemote(statuses))

	require.Equal(t, []Status{
		{Substrate: StoreSubstrate, Backend: "SQLite", Target: path},
		{Substrate: BusSubstrate, Backend: "SQLite", Target: path},
		{Substrate: KVSubstrate, Backend: "SQLite", Target: path},
	}, statuses)
}

func TestUnit_Report_UnusableSettingIsReportedRatherThanGuessedAt(t *testing.T) {
	clearSubstrateEnv(t)
	t.Setenv(PostgresURLEnv, "db:5432/contenox")

	_, err := Report(context.Background(), nil, filepath.Join(t.TempDir(), "local.db"))
	require.Error(t, err)
	require.Contains(t, err.Error(), PostgresURLEnv)
}

func TestUnit_Report_NamesAnUnreachableRemoteInsteadOfTheLocalFile(t *testing.T) {
	clearSubstrateEnv(t)
	t.Setenv(NATSURLEnv, "nats://"+closedLoopbackAddr(t))
	t.Setenv(ValkeyURLEnv, "valkey://"+closedLoopbackAddr(t))
	path := filepath.Join(t.TempDir(), "local.db")

	statuses, err := Report(context.Background(), nil, path)
	require.NoError(t, err)
	require.True(t, AnyRemote(statuses))

	byName := map[string]Status{}
	for _, s := range statuses {
		byName[s.Substrate] = s
	}

	require.Equal(t, "SQLite", byName[StoreSubstrate].Backend)
	require.False(t, byName[StoreSubstrate].Remote())
	require.NoError(t, byName[StoreSubstrate].Err)

	require.Equal(t, "NATS", byName[BusSubstrate].Backend)
	require.Equal(t, NATSURLEnv, byName[BusSubstrate].Setting)
	require.Error(t, byName[BusSubstrate].Err)

	require.Equal(t, "Valkey", byName[KVSubstrate].Backend)
	require.Equal(t, ValkeyURLEnv, byName[KVSubstrate].Setting)
	require.Error(t, byName[KVSubstrate].Err)
}

func TestUnit_Report_ValkeyNamesTheDatabaseAndNamespaceWithoutThePassword(t *testing.T) {
	clearSubstrateEnv(t)
	addr := closedLoopbackAddr(t)
	t.Setenv(ValkeyURLEnv, "valkey://appuser:topsecret@"+addr+"/3?namespace=contenox")

	statuses, err := Report(context.Background(), nil, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)

	for _, s := range statuses {
		require.NotContains(t, s.Target, "topsecret")
		if s.Substrate != KVSubstrate {
			continue
		}
		require.Equal(t, "valkey://appuser:xxxxx@"+addr+"/3?namespace=contenox", s.Target)
		require.Error(t, s.Err)
	}
}

func TestUnit_Report_NeverEchoesANATSToken(t *testing.T) {
	clearSubstrateEnv(t)
	addr := closedLoopbackAddr(t)
	t.Setenv(NATSURLEnv, "nats://topsecret@"+addr)

	statuses, err := Report(context.Background(), nil, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	for _, s := range statuses {
		require.NotContains(t, s.Target, "topsecret")
		if s.Err != nil {
			require.NotContains(t, s.Err.Error(), "topsecret")
		}
	}
	for _, s := range statuses {
		if s.Substrate == BusSubstrate {
			require.Equal(t, "nats://xxxxx@"+addr, s.Target)
			require.Error(t, s.Err)
		}
	}
}

func TestUnit_RedactDSN_NeverEchoesAPassword(t *testing.T) {
	for name, dsn := range map[string]string{
		"url":            "postgres://contenox:topsecret@db:5432/contenox?sslmode=disable",
		"keyword":        "host=db user=contenox password=topsecret dbname=contenox",
		"keyword_quoted": "host=db password='top secret' dbname=contenox",
		"keyword_spaced": "host=db password = topsecret dbname=contenox",
	} {
		t.Run(name, func(t *testing.T) {
			out := redactDSN(dsn)
			require.NotContains(t, out, "topsecret")
			require.NotContains(t, out, "top secret")
			require.Contains(t, out, "db")
		})
	}
}

func TestUnit_RedactDSN_KeepsAPasswordlessDSNReadable(t *testing.T) {
	dsn := "host=db user=contenox dbname=contenox sslmode=disable"
	require.Equal(t, dsn, redactDSN(dsn))
}

func TestUnit_RedactDSN_MasksABareUserinfo(t *testing.T) {
	require.Equal(t, "postgres://xxxxx@db:5432/contenox", redactDSN("postgres://contenox@db:5432/contenox"))
}
