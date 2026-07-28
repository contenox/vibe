package runtimestate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func newDeclaredCapabilityTestDB(t *testing.T) (context.Context, libdb.DBManager, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime-declared.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db, runtimetypes.New(db.WithoutTransaction())
}

// A hand-declared gpt-4o must keep the CanVision the catalog observed for it, since the declared row cannot express vision/think itself.
func TestUnit_RunBackendCycle_DeclaredModelKeepsObservedVision(t *testing.T) {
	ctx, db, store := newDeclaredCapabilityTestDB(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-4o"}},
		})
	}))
	defer server.Close()

	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "openai-backend",
		Name:    "openai",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	data, err := json.Marshal(ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, OpenaiKey, data))

	require.NoError(t, store.AppendModel(ctx, &runtimetypes.Model{
		ID:        "declared-gpt-4o",
		Model:     "gpt-4o",
		CanChat:   true,
		CanPrompt: true,
		CanStream: true,
	}))

	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	state, err := New(ctx, db, bus)
	require.NoError(t, err)
	require.NoError(t, state.RunBackendCycle(ctx))

	rt := state.Get(ctx)
	require.Contains(t, rt, "openai-backend")
	require.Empty(t, rt["openai-backend"].Error)
	require.Len(t, rt["openai-backend"].PulledModels, 1)

	pm := rt["openai-backend"].PulledModels[0]
	require.Equal(t, "gpt-4o", pm.Model)
	require.True(t, pm.CanVision, "declared gpt-4o must keep the observed vision capability")
	require.True(t, pm.CanChat)
	require.True(t, pm.CanStream)
}

// The vLLM reconcile path must read the configured bearer token, send it on the catalog request, and store it on the backend state.
func TestUnit_RunBackendCycle_VLLMPassesConfiguredAuthToken(t *testing.T) {
	ctx, db, store := newDeclaredCapabilityTestDB(t)

	sawAuth := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if sawAuth != "Bearer vllm-secret" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "Qwen/Qwen3-32B", "max_model_len": 32768}},
		})
	}))
	defer server.Close()

	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "vllm-backend",
		Name:    "vllm",
		Type:    "vllm",
		BaseURL: server.URL,
	}))
	data, err := json.Marshal(ProviderConfig{APIKey: "vllm-secret", Type: "vllm"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, OpenaiKey, data))

	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	state, err := New(ctx, db, bus, WithAutoDiscoverModels())
	require.NoError(t, err)
	require.NoError(t, state.RunBackendCycle(ctx))

	require.Equal(t, "Bearer vllm-secret", sawAuth, "catalog listing must carry the configured token")

	rt := state.Get(ctx)
	require.Contains(t, rt, "vllm-backend")
	bs := rt["vllm-backend"]
	require.Empty(t, bs.Error)
	require.Equal(t, "vllm-secret", bs.GetAPIKey(),
		"state must retain the token so the provider adapter hands it to clients")
	require.Len(t, bs.PulledModels, 1)
}

// In the vLLM declared-model merge, observed capabilities are the base and declared trues add on top, so a bare declaration never strips them.
func TestUnit_RunBackendCycle_VLLMDeclaredModelKeepsObservedCaps(t *testing.T) {
	ctx, db, store := newDeclaredCapabilityTestDB(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "Qwen/Qwen3-32B", "max_model_len": 32768}},
		})
	}))
	defer server.Close()

	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "vllm-backend",
		Name:    "vllm",
		Type:    "vllm",
		BaseURL: server.URL,
	}))
	// Declared with chat only; the store requires at least one capability.
	require.NoError(t, store.AppendModel(ctx, &runtimetypes.Model{
		ID:      "declared-qwen",
		Model:   "Qwen/Qwen3-32B",
		CanChat: true,
	}))

	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	state, err := New(ctx, db, bus)
	require.NoError(t, err)
	require.NoError(t, state.RunBackendCycle(ctx))

	rt := state.Get(ctx)
	require.Contains(t, rt, "vllm-backend")
	require.Empty(t, rt["vllm-backend"].Error)
	require.Len(t, rt["vllm-backend"].PulledModels, 1)

	pm := rt["vllm-backend"].PulledModels[0]
	require.True(t, pm.CanChat, "observed chat capability must survive a bare declaration")
	require.True(t, pm.CanStream)
	require.Equal(t, 32768, pm.ContextLength, "observed context length backfills a zero declaration")
}
