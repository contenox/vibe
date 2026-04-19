package contenoxcli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/runtime/backendservice"
	"github.com/contenox/contenox/runtime/internal/clikv"
	"github.com/contenox/contenox/runtime/internal/runtimestate"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/google/uuid"
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

// ---------------------------------------------------------------------------
// setProviderConfigKV
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// backendservice CRUD (replaces the old ensureBackendsFromConfig tests)
// ---------------------------------------------------------------------------

func Test_backendService_create(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	svc := backendservice.New(db)

	b := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "local",
		Type:    "ollama",
		BaseURL: "http://127.0.0.1:11434",
	}
	require.NoError(t, svc.Create(ctx, b))

	list, err := svc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "local", list[0].Name)
	require.Equal(t, "ollama", list[0].Type)
	require.Equal(t, "http://127.0.0.1:11434", list[0].BaseURL)
}

func Test_backendService_update(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	svc := backendservice.New(db)

	b := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "local",
		Type:    "ollama",
		BaseURL: "http://old:11434",
	}
	require.NoError(t, svc.Create(ctx, b))

	b.BaseURL = "http://new:11434"
	require.NoError(t, svc.Update(ctx, b))

	list, err := svc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "http://new:11434", list[0].BaseURL)
}

func Test_backendService_delete(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	svc := backendservice.New(db)

	b := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "local",
		Type:    "ollama",
		BaseURL: "http://127.0.0.1:11434",
	}
	require.NoError(t, svc.Create(ctx, b))
	require.NoError(t, svc.Delete(ctx, b.ID))

	list, err := svc.List(ctx, nil, 10)
	require.NoError(t, err)
	require.Empty(t, list)
}

func Test_backendService_sameTypeAndURL_rejected(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	svc := backendservice.New(db)

	b1 := &runtimetypes.Backend{ID: uuid.NewString(), Name: "a", Type: "ollama", BaseURL: "http://127.0.0.1:11434"}
	b2 := &runtimetypes.Backend{ID: uuid.NewString(), Name: "b", Type: "ollama", BaseURL: "http://127.0.0.1:11434"}

	require.NoError(t, svc.Create(ctx, b1))
	err := svc.Create(ctx, b2)
	require.Error(t, err)
}

func Test_backendService_differentType_sameURL_allowed(t *testing.T) {
	ctx, db, _ := setupSQLiteStore(t)
	svc := backendservice.New(db)

	url := "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project/locations/us-central1"
	b1 := &runtimetypes.Backend{ID: uuid.NewString(), Name: "vertex-google", Type: "vertex-google", BaseURL: url}
	b2 := &runtimetypes.Backend{ID: uuid.NewString(), Name: "vertex-anthropic", Type: "vertex-anthropic", BaseURL: url}

	require.NoError(t, svc.Create(ctx, b1))
	require.NoError(t, svc.Create(ctx, b2))
}

// ---------------------------------------------------------------------------
// getConfigKV / clikv.Prefix (new config cmd helpers)
// ---------------------------------------------------------------------------

func Test_getConfigKV_emptyIfNotSet(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)
	val, err := getConfigKV(ctx, store, "default-model")
	require.NoError(t, err)
	require.Equal(t, "", val)
}

func Test_getConfigKV_roundTrip(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)

	// Simulate what `contenox config set` does.
	data, _ := json.Marshal("qwen2.5:7b")
	require.NoError(t, store.SetKV(ctx, clikv.Prefix+"default-model", data))

	val, err := getConfigKV(ctx, store, "default-model")
	require.NoError(t, err)
	require.Equal(t, "qwen2.5:7b", val)
}

func Test_getConfigKV_multipleKeys(t *testing.T) {
	ctx, _, store := setupSQLiteStore(t)

	for k, v := range map[string]string{
		"default-model":    "phi3:3.8b",
		"default-provider": "ollama",
		"default-chain":    "default-chain.json",
	} {
		data, _ := json.Marshal(v)
		require.NoError(t, store.SetKV(ctx, clikv.Prefix+k, data))
	}

	for k, want := range map[string]string{
		"default-model":    "phi3:3.8b",
		"default-provider": "ollama",
		"default-chain":    "default-chain.json",
	} {
		got, err := getConfigKV(ctx, store, k)
		require.NoError(t, err, "key=%s", k)
		require.Equal(t, want, got, "key=%s", k)
	}
}
