package contenoxcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) (context.Context, libdbexec.DBManager, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config_test.db")
	db, err := libdbexec.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db, runtimetypes.New(db.WithoutTransaction())
}

func TestUnit_getConfigKV_unset_returnsEmpty(t *testing.T) {
	ctx, _, store := openTestDB(t)
	for _, key := range []string{"default-model", "default-provider", "default-alt-model", "default-alt-provider", "default-autocomplete-model", "default-autocomplete-provider", "default-audio-model", "default-audio-provider", "default-max-tokens", "default-think", "default-chain"} {
		val, err := getConfigKV(ctx, store, key)
		require.NoError(t, err, "key=%s", key)
		assert.Equal(t, "", val, "key=%s should be empty when not set", key)
	}
}

func TestUnit_getConfigKV_setAndGet(t *testing.T) {
	ctx, _, store := openTestDB(t)

	data, err := json.Marshal("qwen2.5:7b")
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-model", data))

	val, err := getConfigKV(ctx, store, "default-model")
	require.NoError(t, err)
	assert.Equal(t, "qwen2.5:7b", val)
}

func TestUnit_getConfigKV_allConfigKeys(t *testing.T) {
	ctx, _, store := openTestDB(t)

	pairs := map[string]string{
		"default-model":                 "phi3:3.8b",
		"default-provider":              "ollama",
		"default-alt-model":             "granite-3.2-2b",
		"default-alt-provider":          "vllm",
		"default-autocomplete-model":    "qwen2.5-coder:7b",
		"default-autocomplete-provider": "ollama",
		"default-audio-model":           "gemini-2.5-flash",
		"default-audio-provider":        "gemini",
		"default-max-tokens":            "8192",
		"default-think":                 "medium",
		"default-chain":                 "chain-agent-contenox.json",
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

func TestUnit_getConfigKV_overwrite(t *testing.T) {
	ctx, _, store := openTestDB(t)

	for _, v := range []string{"first", "second", "third"} {
		data, _ := json.Marshal(v)
		require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-model", data))
	}

	val, err := getConfigKV(ctx, store, "default-model")
	require.NoError(t, err)
	assert.Equal(t, "third", val)
}

func TestUnit_normalizeMaxTokensConfig(t *testing.T) {
	got, err := normalizeMaxTokensConfig(" 8192 ")
	require.NoError(t, err)
	assert.Equal(t, "8192", got)

	got, err = normalizeMaxTokensConfig("")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	_, err = normalizeMaxTokensConfig("-1")
	require.Error(t, err)

	_, err = normalizeMaxTokensConfig("many")
	require.Error(t, err)
}

func TestUnit_resolveDBPath_defaultsToGlobalDB(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	expected := filepath.Join(home, ".contenox", "local.db")

	cmd := testCobraCmd()
	dbPath, err := resolveDBPath(cmd)
	require.NoError(t, err)
	assert.Equal(t, expected, dbPath)
}

func TestUnit_resolveDBPath_flagOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	customDB := filepath.Join(dir, "custom.db")

	cmd := testCobraCmd()
	require.NoError(t, cmd.Root().PersistentFlags().Set("db", customDB))

	dbPath, err := resolveDBPath(cmd)
	require.NoError(t, err)
	assert.Equal(t, customDB, dbPath)
}

func testCobraCmd() *cobra.Command {
	root := &cobra.Command{Use: "contenox"}
	root.PersistentFlags().String("db", "", "SQLite database path")
	root.PersistentFlags().String("data-dir", "", "Override the .contenox data directory path")
	child := &cobra.Command{Use: "test"}
	root.AddCommand(child)
	return child
}
