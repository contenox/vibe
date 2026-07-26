package llmrepo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/kernel/llmresolver"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/store/runtimetypes"
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

// reconcileForResolution must only fire for the resolver's no-models / no-match
// errors, and must debounce so a burst of failing requests does not re-scan every
// backend repeatedly.
func TestUnit_ReconcileForResolution_OnlyResolutionErrorsAndDebounced(t *testing.T) {
	ctx, state, _ := newReconcileTestState(t)
	mm := &modelManager{runtime: state, tracker: libtracker.NoopTracker{}}

	// A non-resolution error never triggers a backend cycle.
	if mm.reconcileForResolution(ctx, errors.New("downstream boom")) {
		t.Fatal("non-resolution error should not request a retry")
	}
	if !mm.lastReconcileAt.IsZero() {
		t.Fatal("non-resolution error should not run a backend cycle")
	}

	// A no-models error runs one cycle and asks the caller to retry.
	if !mm.reconcileForResolution(ctx, llmresolver.ErrNoAvailableModels) {
		t.Fatal("ErrNoAvailableModels should run a cycle and request a retry")
	}
	first := mm.lastReconcileAt
	if first.IsZero() {
		t.Fatal("a backend cycle should have run")
	}

	// A second failure inside the debounce window retries against the just-run
	// cycle without scanning every backend again.
	if !mm.reconcileForResolution(ctx, llmresolver.ErrNoSatisfactoryModel) {
		t.Fatal("debounced call should still request a retry")
	}
	if !mm.lastReconcileAt.Equal(first) {
		t.Fatal("debounced call must not run another backend cycle")
	}
}

// When a backend (e.g. modeld) becomes available after the runtime already
// reconciled to an empty state, a resolution failure must re-scan and discover
// it, so the retried request can succeed instead of being stuck on
// "no models found in runtime state".
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

	// Nothing is configured yet: resolution finds no backend at all.
	_, _, _, err := llmresolver.Chat(ctx, req, mm.GetRuntime(ctx), llmresolver.Randomly)
	require.ErrorIs(t, err, llmresolver.ErrNoAvailableModels)

	// The backend comes up after startup.
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

	// The self-heal hook re-scans and discovers it.
	require.True(t, mm.reconcileForResolution(ctx, llmresolver.ErrNoAvailableModels))

	rt := state.Get(ctx)
	require.Contains(t, rt, "openai-backend")
	require.Empty(t, rt["openai-backend"].Error)
	require.Len(t, rt["openai-backend"].PulledModels, 1)
	require.Equal(t, "gpt-5", rt["openai-backend"].PulledModels[0].Model)

	// The retried resolution is no longer blocked on an empty runtime state.
	_, _, _, err = llmresolver.Chat(ctx, req, mm.GetRuntime(ctx), llmresolver.Randomly)
	require.NotErrorIs(t, err, llmresolver.ErrNoAvailableModels)
}
