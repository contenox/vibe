package acpsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	libbus "github.com/contenox/beam/internal/libbus"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_RuntimeStatesTriggersDebouncedReconcile guards against the model
// dropdown going stale (config_options.go's runtimeStates fed Transport.
// modelConfigValues from a raw State.Get read that never triggered a
// reconcile). It mirrors runtimestate's own
// TestUnit_ReconcileIfStale_DiscoversBackendThatAppearsLater, but drives the
// reconcile through the ACP config-options read path instead of calling
// ReconcileIfStale directly, proving that path now self-heals the same way
// GET /state and GET /setup-status do.
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

	// Nothing configured yet: the first config-options read reconciles to an
	// empty snapshot (proving runtimeStates itself drives the initial reconcile).
	require.Empty(t, tr.runtimeStates(ctx))

	// The backend comes up after startup, the way modeld restarting after the
	// runtime would look.
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

	// Still inside the debounce window: a burst of config-options reads (e.g. the
	// ACP client repeatedly asking for the model list) must not force a re-scan.
	require.Empty(t, tr.runtimeStates(ctx))

	// Once the debounce window elapses, reading config options (as the chat
	// model dropdown does) must self-heal and discover the new backend without
	// requiring some unrelated page (GET /state) to have reconciled first.
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
