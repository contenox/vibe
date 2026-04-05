package contenoxcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/clikv"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestDB opens a temp SQLite DB for test assertions.
func openTestDB(t *testing.T) (context.Context, libdbexec.DBManager, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config_test.db")
	db, err := libdbexec.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db, runtimetypes.New(db.WithoutTransaction())
}

// ---------------------------------------------------------------------------
// getConfigKV / config cmd helpers
// ---------------------------------------------------------------------------

func Test_getConfigKV_unset_returnsEmpty(t *testing.T) {
	ctx, _, store := openTestDB(t)
	for _, key := range []string{"default-model", "default-provider", "default-chain"} {
		val, err := getConfigKV(ctx, store, key)
		require.NoError(t, err, "key=%s", key)
		assert.Equal(t, "", val, "key=%s should be empty when not set", key)
	}
}

func Test_getConfigKV_setAndGet(t *testing.T) {
	ctx, _, store := openTestDB(t)

	data, err := json.Marshal("qwen2.5:7b")
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-model", data))

	val, err := getConfigKV(ctx, store, "default-model")
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5:7b", val)
}

func Test_getConfigKV_allConfigKeys(t *testing.T) {
	ctx, _, store := openTestDB(t)

	pairs := map[string]string{
		"default-model":    "phi3:3.8b",
		"default-provider": "ollama",
		"default-chain":    "default-chain.json",
	}
	for k, v := range pairs {
		data, _ := json.Marshal(v)
		require.NoError(t, store.SetKV(ctx, clikv.Prefix+k, data))
	}
	for k, want := range pairs {
		got, err := getConfigKV(ctx, store, k)
		require.NoError(t, err)
		assert.Equal(t, want, got, "key=%s", k)
	}
}

func Test_getConfigKV_overwrite(t *testing.T) {
	ctx, _, store := openTestDB(t)

	for _, v := range []string{"first", "second", "third"} {
		data, _ := json.Marshal(v)
		require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-model", data))
	}

	val, err := getConfigKV(ctx, store, "default-model")
	require.NoError(t, err)
	assert.Equal(t, "third", val)
}

// ---------------------------------------------------------------------------
// resolveDBPath
// ---------------------------------------------------------------------------

func Test_resolveDBPath_defaultsToContenoxDir(t *testing.T) {
	dir := t.TempDir()
	// resolveDBPath prefers a project-local .contenox/ when it exists on disk; otherwise
	// it may fall through to ~/.contenox/local.db if present, which breaks hermetic tests.
	contenoxDir := filepath.Join(dir, ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o755))

	orig, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Build a minimal cobra command that doesn't have --db set.
	cmd := testCobraCmd()
	dbPath, err := resolveDBPath(cmd)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(contenoxDir, "local.db"), dbPath)
}

func Test_resolveDBPath_flagOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	customDB := filepath.Join(dir, "custom.db")

	cmd := testCobraCmd()
	// --db is a persistent flag on the real root command; mirror that here.
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", customDB))

	dbPath, err := resolveDBPath(cmd)
	require.NoError(t, err)
	assert.Equal(t, customDB, dbPath)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// testCobraCmd returns a minimal cobra root command with the --db persistent flag,
// mimicking the subset of rootCmd setup that resolveDBPath needs.
func testCobraCmd() *cobra.Command {
	root := &cobra.Command{Use: "contenox"}
	root.PersistentFlags().String("db", "", "SQLite database path")
	root.PersistentFlags().String("data-dir", "", "Override the .contenox data directory path")
	child := &cobra.Command{Use: "test"}
	root.AddCommand(child)
	return child
}
