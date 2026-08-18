package substrate

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func envOf(vals map[string]string) func(string) string {
	return func(key string) string { return vals[key] }
}

func TestUnit_Resolve_NothingSetSelectsNoBackend(t *testing.T) {
	sel, err := resolveFrom(envOf(nil))
	require.NoError(t, err)
	require.Equal(t, Selection{}, sel)
	require.False(t, sel.UsesPostgres())
	require.False(t, sel.UsesNATS())
	require.False(t, sel.UsesValkey())
}

func TestUnit_Resolve_BlankSettingsSelectNoBackend(t *testing.T) {
	sel, err := resolveFrom(envOf(map[string]string{
		PostgresURLEnv: "   ",
		NATSURLEnv:     "",
		ValkeyURLEnv:   "\t\n",
	}))
	require.NoError(t, err)
	require.Equal(t, Selection{}, sel)
}

func TestUnit_Resolve_PostgresURLNeedsNATSAndValkey(t *testing.T) {
	_, err := resolveFrom(envOf(map[string]string{
		PostgresURLEnv: "postgres://contenox@db:5432/contenox",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), PostgresURLEnv)
	require.Contains(t, err.Error(), NATSURLEnv)
	require.Contains(t, err.Error(), ValkeyURLEnv)
}

func TestUnit_Resolve_PostgresURLNamesTheOneMissingSetting(t *testing.T) {
	_, err := resolveFrom(envOf(map[string]string{
		PostgresURLEnv: "postgres://contenox@db:5432/contenox",
		NATSURLEnv:     "nats://127.0.0.1:4222",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), ValkeyURLEnv)
	require.NotContains(t, err.Error(), NATSURLEnv)
}

func TestUnit_Resolve_FullTripleIsAccepted(t *testing.T) {
	sel, err := resolveFrom(envOf(map[string]string{
		PostgresURLEnv: "postgres://contenox:secret@db:5432/contenox?sslmode=disable",
		NATSURLEnv:     "nats://127.0.0.1:4222",
		ValkeyURLEnv:   "valkey://:vksecret@cache:6379",
	}))
	require.NoError(t, err)
	require.True(t, sel.UsesPostgres())
	require.True(t, sel.UsesNATS())
	require.True(t, sel.UsesValkey())
	require.Equal(t, "cache:6379", sel.ValkeyAddr)
	require.Equal(t, "vksecret", sel.ValkeyPassword)
}

func TestUnit_Resolve_PostgresKeywordDSNIsAccepted(t *testing.T) {
	sel, err := resolveFrom(envOf(map[string]string{
		PostgresURLEnv: "host=db user=contenox dbname=contenox sslmode=disable",
		NATSURLEnv:     "nats://127.0.0.1:4222",
		ValkeyURLEnv:   "cache:6379",
	}))
	require.NoError(t, err)
	require.Equal(t, "host=db user=contenox dbname=contenox sslmode=disable", sel.PostgresDSN)
	require.Equal(t, "cache:6379", sel.ValkeyAddr)
	require.Empty(t, sel.ValkeyPassword)
}

func TestUnit_Resolve_RejectsUnusablePostgresSetting(t *testing.T) {
	for name, value := range map[string]string{
		"no_scheme_no_keywords": "db:5432/contenox",
		"scheme_without_host":   "postgres:///contenox",
		"unparseable":           "postgres://ho st:5432/contenox",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFrom(envOf(map[string]string{PostgresURLEnv: value}))
			require.Error(t, err)
			require.Contains(t, err.Error(), PostgresURLEnv)
		})
	}
}

func TestUnit_Resolve_RejectsUnusableNATSSetting(t *testing.T) {
	for name, value := range map[string]string{
		"no_scheme":     "127.0.0.1:4222",
		"wrong_scheme":  "amqp://127.0.0.1:5672",
		"no_host":       "nats://",
		"empty_in_list": "nats://a:4222,,nats://b:4222",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFrom(envOf(map[string]string{NATSURLEnv: value}))
			require.Error(t, err)
			require.Contains(t, err.Error(), NATSURLEnv)
		})
	}
}

func TestUnit_Resolve_AcceptsNATSServerList(t *testing.T) {
	sel, err := resolveFrom(envOf(map[string]string{
		NATSURLEnv: "nats://a:4222, nats://b:4222",
	}))
	require.NoError(t, err)
	require.True(t, sel.UsesNATS())
}

func TestUnit_Resolve_RejectsUnusableValkeySetting(t *testing.T) {
	for name, value := range map[string]string{
		"bare_host_without_port": "cache",
		"wrong_scheme":           "http://cache:6379",
		"no_host":                "valkey://",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFrom(envOf(map[string]string{ValkeyURLEnv: value}))
			require.Error(t, err)
			require.Contains(t, err.Error(), ValkeyURLEnv)
		})
	}
}

func TestUnit_Resolve_CarriesTheValkeyUserDatabaseAndNamespace(t *testing.T) {
	sel, err := resolveFrom(envOf(map[string]string{
		ValkeyURLEnv: "valkey://appuser:secret@cache:6379/3?namespace=contenox-prod",
	}))
	require.NoError(t, err)
	require.Equal(t, "cache:6379", sel.ValkeyAddr)
	require.Equal(t, "appuser", sel.ValkeyUsername)
	require.Equal(t, "secret", sel.ValkeyPassword)
	require.Equal(t, 3, sel.ValkeyDB)
	require.Equal(t, "contenox-prod", sel.ValkeyNamespace)

	cfg := sel.valkeyConfig()
	require.Equal(t, "cache:6379", cfg.KVAddr)
	require.Equal(t, "appuser", cfg.KVUsername)
	require.Equal(t, "secret", cfg.KVPassword)
	require.Equal(t, 3, cfg.KVDB)
	require.Equal(t, "contenox-prod", cfg.KVNamespace)
}

func TestUnit_Resolve_ValkeyWithoutUserDatabaseOrNamespaceKeepsTheDefaults(t *testing.T) {
	for name, value := range map[string]string{
		"bare_host":       "cache:6379",
		"host_only_url":   "valkey://cache:6379",
		"explicit_zero":   "valkey://cache:6379/0",
		"trailing_slash":  "valkey://cache:6379/",
		"password_only":   "valkey://:vksecret@cache:6379",
		"redis_scheme":    "redis://cache:6379",
		"empty_querymark": "valkey://cache:6379?",
	} {
		t.Run(name, func(t *testing.T) {
			sel, err := resolveFrom(envOf(map[string]string{ValkeyURLEnv: value}))
			require.NoError(t, err)
			require.Equal(t, "cache:6379", sel.ValkeyAddr)
			require.Empty(t, sel.ValkeyUsername)
			require.Zero(t, sel.ValkeyDB)
			require.Empty(t, sel.ValkeyNamespace)
		})
	}
}

func TestUnit_Resolve_RefusesAValkeyDatabaseItCannotHonour(t *testing.T) {
	for name, value := range map[string]string{
		"not_a_number":    "valkey://cache:6379/contenox",
		"negative":        "valkey://cache:6379/-1",
		"nested_path":     "valkey://cache:6379/3/extra",
		"db_in_query":     "valkey://cache:6379?db=3",
		"unknown_query":   "valkey://cache:6379?namespace=ok&pool=4",
		"empty_ns":        "valkey://cache:6379?namespace=",
		"glob_ns":         "valkey://cache:6379?namespace=contenox*",
		"whitespace_ns":   "valkey://cache:6379?namespace=contenox prod",
		"colon_only_ns":   "valkey://cache:6379?namespace=:",
		"nested_db_path":  "valkey://cache:6379/3/4",
		"user_no_pass":    "valkey://appuser@cache:6379",
		"user_empty_pass": "valkey://appuser:@cache:6379",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFrom(envOf(map[string]string{ValkeyURLEnv: value}))
			require.Error(t, err)
			require.Contains(t, err.Error(), ValkeyURLEnv)
		})
	}
}

func TestUnit_Resolve_ValkeyRefusalNeverEchoesThePassword(t *testing.T) {
	_, err := resolveFrom(envOf(map[string]string{
		ValkeyURLEnv: "valkey://appuser:topsecret@cache:6379/nope",
	}))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "topsecret")
}

func TestUnit_Resolve_RefusesValkeyTLSSchemeRatherThanIgnoringIt(t *testing.T) {
	for _, value := range []string{"valkeys://cache:6379", "rediss://cache:6379"} {
		_, err := resolveFrom(envOf(map[string]string{ValkeyURLEnv: value}))
		require.Error(t, err)
		require.Contains(t, err.Error(), "TLS")
	}
}

func TestUnit_Resolve_ErrorDoesNotEchoAPassword(t *testing.T) {
	_, err := resolveFrom(envOf(map[string]string{
		NATSURLEnv: "amqp://contenox:topsecret@broker:5672",
	}))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "topsecret")
}

func TestUnit_Resolve_ErrorDoesNotEchoATokenInTheUsernamePosition(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"nats":                 {NATSURLEnv: "amqp://topsecret@broker:5672"},
		"nats_list":            {NATSURLEnv: "nats://topsecret@a:4222,amqp://topsecret@b:5672"},
		"valkey":               {ValkeyURLEnv: "rediss://topsecret@cache:6379"},
		"valkey_userinfo_only": {ValkeyURLEnv: "valkey://topsecret@cache:6379"},
		"postgres":             {PostgresURLEnv: "postgres://topsecret@/contenox"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFrom(envOf(env))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "topsecret")
		})
	}
}

func TestUnit_Redact_MasksACredentialInEitherUserinfoPosition(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql", "nats", "tls", "ws", "wss", "valkey", "redis"} {
		for position, raw := range map[string]string{
			"username": scheme + "://topsecret@host:4222/db",
			"password": scheme + "://contenox:topsecret@host:4222/db",
			"userless": scheme + "://:topsecret@host:4222/db",
		} {
			t.Run(scheme+"_"+position, func(t *testing.T) {
				out := redact(raw)
				require.NotContains(t, out, "topsecret")
				require.Contains(t, out, maskedCredential)
				require.Contains(t, out, "host:4222", "the server it names stays readable")
			})
		}
	}
}

func TestUnit_Redact_MasksEveryEntryOfAServerList(t *testing.T) {
	for name, raw := range map[string]string{
		"tokens":            "nats://topsecret@a:4222,nats://topsecret@b:4222",
		"tokens_spaced":     "nats://topsecret@a:4222, nats://topsecret@b:4222",
		"passwords_spaced":  "nats://contenox:topsecret@a:4222, nats://contenox:topsecret@b:4222",
		"mixed_schemes":     "tls://topsecret@a:4222,ws://topsecret@b:8080",
		"second_entry_only": "nats://a:4222,nats://topsecret@b:4222",
	} {
		t.Run(name, func(t *testing.T) {
			out := redact(raw)
			require.NotContains(t, out, "topsecret")
			require.Contains(t, out, "a:4222")
		})
	}
}

func TestUnit_Redact_MasksAURLItCannotParse(t *testing.T) {
	for name, raw := range map[string]string{
		"space_in_credential": "nats://top secret@bus:4222",
		"space_and_username":  "nats://contenox:top secret@bus:4222",
		"comma_in_password":   "postgres://contenox:top,secret@db:5432/contenox",
		"multi_host":          "postgres://contenox:topsecret@a:5432,b:5432/contenox",
	} {
		t.Run(name, func(t *testing.T) {
			out := redact(raw)
			require.NotContains(t, out, "topsecret")
			require.NotContains(t, out, "top secret")
			require.NotContains(t, out, "top,secret")
			require.Contains(t, out, maskedCredential)
		})
	}
}

func TestUnit_Redact_MasksAPasswordContainingURLDelimiters(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		leaked  string
		surface string
	}{
		"slash": {"nats://contenox:top/secret@bus:4222", "top/secret", "bus:4222"},
		"query": {"postgres://contenox:top?secret@db:5432/contenox", "top?secret", "db:5432"},
		"hash":  {"valkey://contenox:top#secret@cache:6379", "top#secret", "cache:6379"},
	} {
		t.Run(name, func(t *testing.T) {
			out := redact(tc.raw)
			require.NotContains(t, out, tc.leaked)
			require.Contains(t, out, maskedCredential)
			require.Contains(t, out, tc.surface, "the server it names stays readable")
		})
	}
}

func TestUnit_Resolve_ErrorFromAnUnparseableURLDoesNotEchoThePassword(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"nats":     {NATSURLEnv: "nats://contenox:top/secret@bus:4222"},
		"postgres": {PostgresURLEnv: "postgres://contenox:top?secret@db:5432/contenox"},
		"valkey":   {ValkeyURLEnv: "valkey://contenox:top#secret@cache:6379"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolveFrom(envOf(env))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "top/secret")
			require.NotContains(t, err.Error(), "top?secret")
			require.NotContains(t, err.Error(), "top#secret")
		})
	}
}

func TestUnit_Redact_LeavesAURLWithoutUserinfoAlone(t *testing.T) {
	for _, raw := range []string{
		"nats://bus:4222",
		"nats://a:4222, nats://b:4222",
		"postgres://db:5432/contenox?sslmode=disable",
		"cache:6379",
		"host=db user=contenox dbname=contenox",
	} {
		require.Equal(t, raw, redact(raw))
	}
}

func TestUnit_Configured_IsTrueOnlyWhenASettingNamesABackend(t *testing.T) {
	require.False(t, configuredFrom(envOf(nil)))
	require.False(t, configuredFrom(envOf(map[string]string{PostgresURLEnv: "  ", NATSURLEnv: "\t"})))
	for _, key := range []string{PostgresURLEnv, NATSURLEnv, ValkeyURLEnv} {
		require.True(t, configuredFrom(envOf(map[string]string{key: "anything"})), key)
	}
}

func clearSubstrateEnv(t *testing.T) {
	t.Helper()
	t.Setenv(PostgresURLEnv, "")
	t.Setenv(NATSURLEnv, "")
	t.Setenv(ValkeyURLEnv, "")
}

func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

func TestUnit_OpenDB_UnsetOpensTheSQLiteFile(t *testing.T) {
	clearSubstrateEnv(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "local.db")

	db, err := OpenDB(ctx, path, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = os.Stat(path)
	require.NoError(t, err)

	var one int
	require.NoError(t, db.WithoutTransaction().QueryRowContext(ctx, `SELECT COUNT(*) FROM kv_store`).Scan(&one))
}

func TestUnit_OpenBus_UnsetUsesTheDatabaseBackedBus(t *testing.T) {
	clearSubstrateEnv(t)
	ctx := context.Background()

	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "local.db"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	bus, err := OpenBus(ctx, db.WithoutTransaction())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bus.Close()) })

	require.NoError(t, bus.Publish(ctx, "substrate.test", []byte("ping")))
}

func TestUnit_OpenKV_UnsetUsesTheDatabaseAndNeverClosesIt(t *testing.T) {
	clearSubstrateEnv(t)
	ctx := context.Background()

	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "local.db"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	kv, releaseKV, err := OpenKV(ctx, db)
	require.NoError(t, err)

	exec, err := kv.Executor(ctx)
	require.NoError(t, err)
	require.NoError(t, exec.Set(ctx, "substrate:test", json.RawMessage(`{"ok":true}`)))

	releaseKV()

	var one int
	require.NoError(t, db.WithoutTransaction().QueryRowContext(ctx, `SELECT COUNT(*) FROM kv_store`).Scan(&one))
	require.Equal(t, 1, one)
}

func TestUnit_OpenHelpers_RefuseAnUnusableSetting(t *testing.T) {
	clearSubstrateEnv(t)
	t.Setenv(PostgresURLEnv, "db:5432/contenox")
	ctx := context.Background()

	_, err := OpenDB(ctx, filepath.Join(t.TempDir(), "local.db"), false)
	require.Error(t, err)
	require.Contains(t, err.Error(), PostgresURLEnv)

	_, err = OpenBus(ctx, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), PostgresURLEnv)

	_, _, err = OpenKV(ctx, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), PostgresURLEnv)
}

func TestUnit_OpenDB_ExplicitPathWithPostgresRefusesInsteadOfDropping(t *testing.T) {
	clearSubstrateEnv(t)
	t.Setenv(PostgresURLEnv, "postgres://contenox@db:5432/contenox")
	t.Setenv(NATSURLEnv, "nats://bus:4222")
	t.Setenv(ValkeyURLEnv, "valkey://cache:6379")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "local.db")

	_, err := OpenDB(ctx, path, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), PostgresURLEnv)
	require.Contains(t, err.Error(), path)
	require.NoFileExists(t, path)
}
