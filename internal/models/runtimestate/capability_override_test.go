package runtimestate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/modelcapability"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

func newCapabilityOverrideTestState(t *testing.T) (context.Context, *State, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime-capability.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return ctx, &State{dbInstance: db}, runtimetypes.New(db.WithoutTransaction())
}

func TestUnit_ApplyCapabilityOverrides_ManualTrueWins(t *testing.T) {
	ctx, state, store := newCapabilityOverrideTestState(t)
	_, err := modelcapability.New(store).SetThink(ctx, "OpenAI", "gpt-5", true)
	require.NoError(t, err)

	got := state.applyCapabilityOverrides(ctx, "openai", ModelPullStatus{Model: "gpt-5"})
	require.True(t, got.CanThink)
}

func TestUnit_ApplyCapabilityOverrides_ManualFalseSuppressesProviderTrue(t *testing.T) {
	ctx, state, store := newCapabilityOverrideTestState(t)
	_, err := modelcapability.New(store).SetThink(ctx, "vllm", "Qwen/Qwen3-32B", false)
	require.NoError(t, err)

	got := state.applyCapabilityOverrides(ctx, "vllm", ModelPullStatus{
		Model:    "Qwen/Qwen3-32B",
		CanThink: true,
	})
	require.False(t, got.CanThink)
}

func TestUnit_RunBackendCycle_AppliesOpenAIManualThinkOverride(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/models", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "runtime-cycle.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "openai-backend",
		Name:    "openai",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	data, err := json.Marshal(ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, OpenaiKey, data))
	_, err = modelcapability.New(store).SetThink(ctx, "openai", "gpt-5", true)
	require.NoError(t, err)

	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	state, err := New(ctx, db, bus, WithAutoDiscoverModels())
	require.NoError(t, err)
	require.NoError(t, state.RunBackendCycle(ctx))

	rt := state.Get(ctx)
	require.Contains(t, rt, "openai-backend")
	require.Empty(t, rt["openai-backend"].Error)
	require.Len(t, rt["openai-backend"].PulledModels, 1)
	require.Equal(t, "gpt-5", rt["openai-backend"].PulledModels[0].Model)
	require.True(t, rt["openai-backend"].PulledModels[0].CanThink)
}
