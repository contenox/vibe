package vibecli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/internal/runtimestate"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/stretchr/testify/require"
)

func setupSQLiteStore(t *testing.T) (context.Context, libdb.DBManager, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := runtimetypes.New(db.WithoutTransaction())
	return ctx, db, store
}

func Test_isUniqueConstraintBaseURLError(t *testing.T) {
	require.True(t, isUniqueConstraintBaseURLError(fmt.Errorf("constraint failed: UNIQUE constraint failed: llm_backends.base_url (2067)")))
	require.True(t, isUniqueConstraintBaseURLError(fmt.Errorf("libdb: unexpected database error: UNIQUE constraint failed: llm_backends.base_url")))
	require.False(t, isUniqueConstraintBaseURLError(nil))
	require.False(t, isUniqueConstraintBaseURLError(fmt.Errorf("other error")))
	require.False(t, isUniqueConstraintBaseURLError(fmt.Errorf("UNIQUE constraint failed: llm_backends.name")))
}

func Test_setProviderConfigKV(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)

	err := setProviderConfigKV(ctx, store, "openai", "sk-test-key")
	require.NoError(t, err)

	var pc runtimestate.ProviderConfig
	err = store.GetKV(ctx, runtimestate.ProviderKeyPrefix+"openai", &pc)
	require.NoError(t, err)
	require.Equal(t, "openai", pc.Type)
	require.Equal(t, "sk-test-key", pc.APIKey)
}

func Test_setProviderConfigKV_gemini_keyFormat(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)

	err := setProviderConfigKV(ctx, store, "Gemini", "gemini-secret")
	require.NoError(t, err)

	key := runtimestate.ProviderKeyPrefix + "gemini"
	var pc runtimestate.ProviderConfig
	err = store.GetKV(ctx, key, &pc)
	require.NoError(t, err)
	require.Equal(t, "Gemini", pc.Type)
	require.Equal(t, "gemini-secret", pc.APIKey)
}

func Test_ensureBackendsFromConfig_createsNewBackend(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	backendSvc := backendservice.New(db)

	resolved, _, _ := resolveEffectiveBackends(localConfig{}, "http://127.0.0.1:11434", "phi3:3.8b")
	require.Len(t, resolved, 1)

	err := ensureBackendsFromConfig(ctx, db, backendSvc, resolved)
	require.NoError(t, err)

	list, err := backendSvc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "default", list[0].Name)
	require.Equal(t, "ollama", list[0].Type)
	require.Equal(t, "http://127.0.0.1:11434", list[0].BaseURL)
}

func Test_ensureBackendsFromConfig_updatesExistingBackend(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	backendSvc := backendservice.New(db)

	// Create initial backend
	resolved1, _, _ := resolveEffectiveBackends(localConfig{}, "http://old:11434", "old-model")
	require.NoError(t, ensureBackendsFromConfig(ctx, db, backendSvc, resolved1))

	// Resolve with new URL (same backend name)
	cfg := localConfig{
		Backends: []backendEntry{
			{Name: "default", Type: "ollama", BaseURL: "http://new:11434"},
		},
	}
	resolved2, _, _ := resolveEffectiveBackends(cfg, "http://ignored:11434", "ignored")
	require.NoError(t, ensureBackendsFromConfig(ctx, db, backendSvc, resolved2))

	list, err := backendSvc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "http://new:11434", list[0].BaseURL)
}

func Test_ensureBackendsFromConfig_skipsEmptyBaseURL(t *testing.T) {
	ctx, db, store := setupSQLiteStore(t)
	backendSvc := backendservice.New(db)

	// One valid, one empty baseURL - we need to build resolved manually for empty.
	// resolveEffectiveBackends with empty Backends gives one backend from effectiveOllama.
	// To get a backend with empty baseURL we need cfg.Backends with one entry that has empty BaseURL.
	cfg := localConfig{
		Backends: []backendEntry{
			{Name: "empty", Type: "ollama", BaseURL: ""},
			{Name: "valid", Type: "ollama", BaseURL: "http://127.0.0.1:11434"},
		},
	}
	resolved, _, _ := resolveEffectiveBackends(cfg, "http://x:11434", "m")
	require.NoError(t, ensureBackendsFromConfig(ctx, db, backendSvc, resolved))

	list, err := backendSvc.List(ctx, nil, 10)
	require.NoError(t, err)
	// Only "valid" should be created; "empty" is skipped
	require.Len(t, list, 1)
	require.Equal(t, "valid", list[0].Name)

	_ = store
}

func Test_ensureBackendsFromConfig_sameBaseURLDifferentName_updatesExisting(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	backendSvc := backendservice.New(db)

	// Create a backend with name "ollama" (e.g. from an old config).
	resolved1, _, _ := resolveEffectiveBackends(localConfig{
		Backends: []backendEntry{{Name: "ollama", Type: "ollama", BaseURL: "http://127.0.0.1:11434"}},
	}, "http://x:11434", "m")
	require.NoError(t, ensureBackendsFromConfig(ctx, db, backendSvc, resolved1))

	// Config now wants "default" with the same base_url (DB has UNIQUE on base_url).
	resolved2, _, _ := resolveEffectiveBackends(localConfig{}, "http://127.0.0.1:11434", "phi3:3.8b")
	require.NoError(t, ensureBackendsFromConfig(ctx, db, backendSvc, resolved2))

	list, err := backendSvc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "default", list[0].Name)
	require.Equal(t, "http://127.0.0.1:11434", list[0].BaseURL)
}

func Test_ensureBackendsFromConfig_storesOpenAIKeyInKV(t *testing.T) {
	ctx, db, store := setupSQLiteStore(t)
	backendSvc := backendservice.New(db)

	cfg := localConfig{
		Backends: []backendEntry{
			{Name: "openai", Type: "openai", BaseURL: "https://api.openai.com", APIKey: "sk-test"},
		},
	}
	resolved, _, _ := resolveEffectiveBackends(cfg, "http://x:11434", "m")
	require.NoError(t, ensureBackendsFromConfig(ctx, db, backendSvc, resolved))

	var pc runtimestate.ProviderConfig
	err := store.GetKV(ctx, runtimestate.ProviderKeyPrefix+"openai", &pc)
	require.NoError(t, err)
	require.Equal(t, "sk-test", pc.APIKey)
}

func Test_setProviderConfigKV_valueRoundTrip(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)

	key := runtimestate.ProviderKeyPrefix + "openai"
	pc := runtimestate.ProviderConfig{APIKey: "roundtrip-key", Type: "openai"}
	data, err := json.Marshal(pc)
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, key, data))

	var out runtimestate.ProviderConfig
	require.NoError(t, store.GetKV(ctx, key, &out))
	require.Equal(t, pc.APIKey, out.APIKey)
	require.Equal(t, pc.Type, out.Type)
}
