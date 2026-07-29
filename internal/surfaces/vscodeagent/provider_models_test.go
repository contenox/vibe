package vscodeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// A vertex-google backend (as `contenox backend add --type vertex-google`
// creates, and as CLI/serve require to reach Vertex at all) must be reported as
// configured through the bridge — the extension keys its "(not configured)"
// label off providerservice-derived status, not a hand-maintained provider list.
func TestListProvidersReportsVertexGoogleConfigured(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		Name:    "vertex",
		Type:    "vertex-google",
		BaseURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1",
	}))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-provider", "vertex-google"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-model", "gemini-2.5-flash"))

	responses := runServerRequests(t, ctx, server,
		rpcRequest(1, "listProviders", nil),
		rpcRequest(2, "health", nil),
	)

	var providers listProvidersResult
	require.NoError(t, json.Unmarshal(responses[0].Result, &providers))
	var vertex *providerInfo
	for i := range providers.Providers {
		if providers.Providers[i].Provider == "vertex-google" {
			vertex = &providers.Providers[i]
		}
	}
	require.NotNil(t, vertex, "vertex-google must be present in the supported-provider catalog")
	require.True(t, vertex.Configured, "vertex-google with a registered backend must report configured")
	require.Equal(t, "vertex", vertex.BackendName)

	var health healthResult
	require.NoError(t, json.Unmarshal(responses[1].Result, &health))
	require.True(t, health.Configured)
}

// The picker must surface a provider's live catalog, not only persisted model
// rows. Vertex models come solely from the live publisher catalog (which needs
// gcloud ADC and cannot be exercised offline), so a fake OpenAI-compatible
// backend stands in for "a reachable cloud backend whose models exist only in
// its live catalog" and drives the exact same runtimestate cycle the fix uses.
func TestListModelsSurfacesLiveBackendCatalog(t *testing.T) {
	ctx, server, store := newTestServer(t)

	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-live-1"}},
		})
	}))
	defer catalog.Close()

	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "openai-backend",
		Name:    "openai",
		Type:    "openai",
		BaseURL: catalog.URL,
	}))
	keyData, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, keyData))

	// Nothing is persisted in the models table: the model can only be found by
	// reconciling the live backend, which is precisely what regressed for Vertex.
	responses := runServerRequests(t, ctx, server,
		rpcRequest(1, "listModels", map[string]string{"provider": "openai"}),
	)
	var models listModelsResult
	require.NoError(t, json.Unmarshal(responses[0].Result, &models))
	requireModel(t, models.Models, "gpt-live-1", "openai", "observed")

	_ = context.Background
}
