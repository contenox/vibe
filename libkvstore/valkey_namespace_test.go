package libkvstore_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	libkv "github.com/contenox/contenox/libkvstore"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func valkeyAddr(t *testing.T, ctx context.Context) (string, testcontainers.Container) {
	t.Helper()
	connStr, container, cleanup, err := SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	u, err := url.Parse(connStr)
	require.NoError(t, err)
	return u.Host, container
}

func valkeyExecutor(t *testing.T, ctx context.Context, cfg libkv.Config) libkv.KVExecutor {
	t.Helper()
	mgr, err := libkv.NewManager(cfg, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	exec, err := mgr.Executor(ctx)
	require.NoError(t, err)
	return exec
}

func TestUnit_ValkeyNamespaceIsolatesTheKeyspace(t *testing.T) {
	requireValkey(t)
	ctx := context.Background()
	addr, _ := valkeyAddr(t, ctx)

	alpha := valkeyExecutor(t, ctx, libkv.Config{KVAddr: addr, KVNamespace: "alpha"})
	beta := valkeyExecutor(t, ctx, libkv.Config{KVAddr: addr, KVNamespace: "beta"})
	raw := valkeyExecutor(t, ctx, libkv.Config{KVAddr: addr})

	require.NoError(t, alpha.Set(ctx, "prov:backend-a", json.RawMessage(`"alpha"`)))
	require.NoError(t, beta.Set(ctx, "prov:backend-a", json.RawMessage(`"beta"`)))

	got, err := alpha.Get(ctx, "prov:backend-a")
	require.NoError(t, err)
	require.JSONEq(t, `"alpha"`, string(got))

	keys, err := alpha.Keys(ctx, "prov:*")
	require.NoError(t, err)
	require.Equal(t, []libkv.Key{"prov:backend-a"}, keys, "Keys must return the caller's own key, not the namespaced one")

	onWire, err := raw.Keys(ctx, "*")
	require.NoError(t, err)
	require.ElementsMatch(t, []libkv.Key{"alpha:prov:backend-a", "beta:prov:backend-a"}, onWire)

	require.NoError(t, alpha.Delete(ctx, "prov:backend-a"))
	exists, err := beta.Exists(ctx, "prov:backend-a")
	require.NoError(t, err)
	require.True(t, exists, "one namespace must not delete another's key")
}

func TestUnit_ValkeyDatabaseIndexIsHonoured(t *testing.T) {
	requireValkey(t)
	ctx := context.Background()
	addr, _ := valkeyAddr(t, ctx)

	zero := valkeyExecutor(t, ctx, libkv.Config{KVAddr: addr})
	three := valkeyExecutor(t, ctx, libkv.Config{KVAddr: addr, KVDB: 3})

	require.NoError(t, zero.Set(ctx, "presence:host:one", json.RawMessage(`"db0"`)))
	require.NoError(t, three.Set(ctx, "presence:host:one", json.RawMessage(`"db3"`)))

	got, err := zero.Get(ctx, "presence:host:one")
	require.NoError(t, err)
	require.JSONEq(t, `"db0"`, string(got))

	got, err = three.Get(ctx, "presence:host:one")
	require.NoError(t, err)
	require.JSONEq(t, `"db3"`, string(got))

	require.NoError(t, three.Delete(ctx, "presence:host:one"))
	exists, err := zero.Exists(ctx, "presence:host:one")
	require.NoError(t, err)
	require.True(t, exists, "a write against database 3 must not touch database 0")
}

func TestUnit_ValkeyUsernameAuthenticatesAsThatUser(t *testing.T) {
	requireValkey(t)
	ctx := context.Background()
	addr, container := valkeyAddr(t, ctx)

	code, _, err := container.Exec(ctx, []string{
		"valkey-cli", "ACL", "SETUSER", "appuser", "on", ">secret", "~contenox:*", "+@all",
	})
	require.NoError(t, err)
	require.Zero(t, code)

	scoped := valkeyExecutor(t, ctx, libkv.Config{
		KVAddr:      addr,
		KVUsername:  "appuser",
		KVPassword:  "secret",
		KVNamespace: "contenox",
	})
	require.NoError(t, scoped.Set(ctx, "prov:backend-a", json.RawMessage(`"ok"`)))
	got, err := scoped.Get(ctx, "prov:backend-a")
	require.NoError(t, err)
	require.JSONEq(t, `"ok"`, string(got))

	mgr, err := libkv.NewManager(libkv.Config{
		KVAddr:     addr,
		KVUsername: "appuser",
		KVPassword: "wrong",
	}, 0)
	if err == nil {
		t.Cleanup(func() { _ = mgr.Close() })
		exec, execErr := mgr.Executor(ctx)
		if execErr == nil {
			_, execErr = exec.Get(ctx, "prov:backend-a")
		}
		err = execErr
	}
	require.Error(t, err, "a wrong password for the named user must not connect")
}
