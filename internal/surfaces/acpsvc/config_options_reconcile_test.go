package acpsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/kernel/enginesvc"
	libbus "github.com/contenox/contenox/internal/libbus"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_RuntimeStatesTriggersDebouncedReconcile pins: runtimeStates self-heals a stale backend list, same as GET /state.
func TestUnit_RuntimeStatesTriggersDebouncedReconcile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "acp-runtime-states-reconcile.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	bus := libbus.NewSQLite(db.WithoutTransaction())
	t.Cleanup(func() { _ = bus.Close() })
	state, err := runtimestate.New(ctx, db, bus, runtimestate.WithAutoDiscoverModels())
	require.NoError(t, err)

	tr := &Transport{deps: Deps{Engine: &enginesvc.Engine{State: state}}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	defer server.Close()

	require.Empty(t, tr.runtimeStates(ctx))

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "openai-backend",
		Name:    "openai-backend",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	keyData, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, keyData))

	// Inside the debounce window: a burst of reads must not force a re-scan.
	require.Empty(t, tr.runtimeStates(ctx))

	original := runtimestate.ReconcileDebounceInterval
	runtimestate.ReconcileDebounceInterval = 0
	t.Cleanup(func() { runtimestate.ReconcileDebounceInterval = original })

	states := tr.runtimeStates(ctx)
	require.NotEmpty(t, states)
	var found bool
	for _, st := range states {
		if st.Name == "openai-backend" {
			found = true
			require.Empty(t, st.Error)
			require.Len(t, st.PulledModels, 1)
			require.Equal(t, "gpt-5", st.PulledModels[0].Model)
		}
	}
	require.True(t, found, "expected openai-backend to be discovered via runtimeStates after reconcile")
}
