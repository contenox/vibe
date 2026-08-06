package llmrepo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func newReconcileTestState(t *testing.T, opts ...runtimestate.Option) (context.Context, *runtimestate.State, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "llmrepo-reconcile.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	state, err := runtimestate.New(ctx, db, bus, opts...)
	require.NoError(t, err)
	return ctx, state, db
}

// reconcileForResolution must only fire for the resolver's no-models/no-match errors, and must debounce repeated failures.
func TestUnit_ReconcileForResolution_OnlyResolutionErrorsAndDebounced(t *testing.T) {
	ctx, state, _ := newReconcileTestState(t)
	mm := &modelManager{runtime: state, tracker: libtracker.NoopTracker{}}

	if mm.reconcileForResolution(ctx, errors.New("downstream boom")) {
		t.Fatal("non-resolution error should not request a retry")
	}
	if !mm.lastReconcileAt.IsZero() {
		t.Fatal("non-resolution error should not run a backend cycle")
	}

	if !mm.reconcileForResolution(ctx, llmresolver.ErrNoAvailableModels) {
		t.Fatal("ErrNoAvailableModels should run a cycle and request a retry")
	}
	first := mm.lastReconcileAt
	if first.IsZero() {
		t.Fatal("a backend cycle should have run")
	}

	if !mm.reconcileForResolution(ctx, llmresolver.ErrNoSatisfactoryModel) {
		t.Fatal("debounced call should still request a retry")
	}
	if !mm.lastReconcileAt.Equal(first) {
		t.Fatal("debounced call must not run another backend cycle")
	}
}

// When a backend becomes available after the runtime reconciled to an empty state, a resolution failure must re-scan and discover it.
func TestUnit_ReconcileForResolution_DiscoversBackendThatAppearsLater(t *testing.T) {
	ctx, state, db := newReconcileTestState(t, runtimestate.WithAutoDiscoverModels())
	mm := &modelManager{runtime: state, tracker: libtracker.NoopTracker{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	defer server.Close()

	req := llmresolver.Request{ProviderTypes: []string{"openai"}, ModelNames: []string{"gpt-5"}}

	_, _, _, err := llmresolver.Chat(ctx, req, mm.GetRuntime(ctx), llmresolver.Randomly)
	require.ErrorIs(t, err, llmresolver.ErrNoAvailableModels)

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "openai-backend",
		Name:    "openai",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	keyData, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, keyData))

	require.True(t, mm.reconcileForResolution(ctx, llmresolver.ErrNoAvailableModels))

	rt := state.Get(ctx)
	require.Contains(t, rt, "openai-backend")
	require.Empty(t, rt["openai-backend"].Error)
	require.Len(t, rt["openai-backend"].PulledModels, 1)
	require.Equal(t, "gpt-5", rt["openai-backend"].PulledModels[0].Model)

	_, _, _, err = llmresolver.Chat(ctx, req, mm.GetRuntime(ctx), llmresolver.Randomly)
	require.NotErrorIs(t, err, llmresolver.ErrNoAvailableModels)
}
