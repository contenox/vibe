package runtimestate_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/stretchr/testify/require"
)

// newTestKV returns a SQLite-backed KV manager over a temp-file DB. A file (not
// ":memory:") is used so the internal Executor calls inside Clear/Invalidate see
// the same database as the test's own writes.
func newTestKV(t *testing.T) libkvstore.KVManager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	db, err := libdbexec.NewSQLiteDBManager(context.Background(), path, libkvstore.SQLiteSchema)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return libkvstore.NewSQLiteManager(db)
}

func TestUnit_ClearModelCache_removesProvKeysOnly(t *testing.T) {
	ctx := context.Background()
	kv := newTestKV(t)
	exec, err := kv.Executor(ctx)
	require.NoError(t, err)

	val, _ := json.Marshal(map[string]any{"models": []string{}, "api_key": ""})
	require.NoError(t, exec.Set(ctx, "prov:backend-a", val))
	require.NoError(t, exec.Set(ctx, "prov:backend-b", val))
	require.NoError(t, exec.Set(ctx, "cloud-provider:openai", val)) // provider config — must survive

	n, err := runtimestate.ClearModelCache(ctx, kv)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	for _, k := range []string{"prov:backend-a", "prov:backend-b"} {
		exists, err := exec.Exists(ctx, k)
		require.NoError(t, err)
		require.False(t, exists, "%s should have been cleared", k)
	}
	exists, err := exec.Exists(ctx, "cloud-provider:openai")
	require.NoError(t, err)
	require.True(t, exists, "non-model-cache keys must be left untouched")
}

func TestUnit_InvalidateModelCache_removesOnlyOneBackend(t *testing.T) {
	ctx := context.Background()
	kv := newTestKV(t)
	exec, err := kv.Executor(ctx)
	require.NoError(t, err)

	val, _ := json.Marshal(map[string]any{})
	require.NoError(t, exec.Set(ctx, "prov:backend-a", val))
	require.NoError(t, exec.Set(ctx, "prov:backend-b", val))

	require.NoError(t, runtimestate.InvalidateModelCache(ctx, kv, "backend-a"))

	existsA, err := exec.Exists(ctx, "prov:backend-a")
	require.NoError(t, err)
	require.False(t, existsA)
	existsB, err := exec.Exists(ctx, "prov:backend-b")
	require.NoError(t, err)
	require.True(t, existsB)
}

func TestUnit_CacheHelpers_nilKVAreNoOps(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, runtimestate.InvalidateModelCache(ctx, nil, "x"))
	n, err := runtimestate.ClearModelCache(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}
