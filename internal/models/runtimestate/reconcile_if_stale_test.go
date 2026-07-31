package runtimestate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// claimReconcile is the debounce gate behind ReconcileIfStale: the first call
// claims the window, calls inside the window are skipped, and a call past the
// window claims again.
func TestUnit_ClaimReconcile_Debounces(t *testing.T) {
	s := &State{}
	t0 := time.Now()

	if !s.claimReconcile(t0) {
		t.Fatal("first reconcile (zero clock) must be claimed")
	}
	if s.claimReconcile(t0.Add(ReconcileDebounceInterval / 2)) {
		t.Fatal("a call inside the debounce window must be skipped")
	}
	if !s.claimReconcile(t0.Add(ReconcileDebounceInterval)) {
		t.Fatal("a call past the debounce window must be claimed again")
	}
}

func newReconcileStateTest(t *testing.T, opts ...Option) (context.Context, *State, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reconcile-if-stale.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	state, err := New(ctx, db, bus, opts...)
	require.NoError(t, err)
	return ctx, state, db
}

// A read-path ReconcileIfStale must discover a backend that comes up after an
// empty reconcile, but only once the debounce window has elapsed.
func TestUnit_ReconcileIfStale_DiscoversBackendThatAppearsLater(t *testing.T) {
	ctx, state, db := newReconcileStateTest(t, WithAutoDiscoverModels())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	defer server.Close()

	require.NoError(t, state.ReconcileIfStale(ctx))
	require.Empty(t, state.Get(ctx))

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "openai-backend",
		Name:    "openai",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	keyData, err := json.Marshal(ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, OpenaiKey, keyData))

	// A read inside the debounce window is a no-op; the new backend stays invisible.
	require.NoError(t, state.ReconcileIfStale(ctx))
	require.NotContains(t, state.Get(ctx), "openai-backend")

	original := ReconcileDebounceInterval
	ReconcileDebounceInterval = 0
	t.Cleanup(func() { ReconcileDebounceInterval = original })

	require.NoError(t, state.ReconcileIfStale(ctx))
	rt := state.Get(ctx)
	require.Contains(t, rt, "openai-backend")
	require.Empty(t, rt["openai-backend"].Error)
	require.Len(t, rt["openai-backend"].PulledModels, 1)
	require.Equal(t, "gpt-5", rt["openai-backend"].PulledModels[0].Model)
}
