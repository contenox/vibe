package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_CatalogProvider_VisionFromImageInputCapability asserts that CanVision
// is derived solely from the model resource's capabilities.image_input.supported
// field reported by GET /v1/models, never from the model name.
func TestUnit_CatalogProvider_VisionFromImageInputCapability(t *testing.T) {
	const body = `{
		"data": [
			{
				"id": "claude-opus-4-6",
				"created_at": "2026-02-04T00:00:00Z",
				"max_input_tokens": 200000,
				"max_tokens": 64000,
				"capabilities": {
					"image_input": {"supported": true},
					"thinking": {"supported": true}
				}
			},
			{
				"id": "claude-haiku-text-only",
				"created_at": "2026-02-04T00:00:00Z",
				"max_input_tokens": 100000,
				"max_tokens": 8000,
				"capabilities": {
					"image_input": {"supported": false}
				}
			},
			{
				"id": "claude-no-capabilities-object"
			}
		],
		"has_more": false,
		"last_id": "claude-no-capabilities-object"
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "test-key", r.Header.Get("x-api-key"))
		require.Equal(t, anthropicAPIVersion, r.Header.Get("anthropic-version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "anthropic",
		BaseURL: server.URL,
		APIKey:  "test-key",
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 3)

	byName := map[string]modelrepo.ObservedModel{}
	for _, m := range models {
		byName[m.Name] = m
	}

	require.True(t, byName["claude-opus-4-6"].CanVision, "image_input.supported=true must set CanVision")
	require.False(t, byName["claude-haiku-text-only"].CanVision, "image_input.supported=false must leave CanVision unset")
	require.False(t, byName["claude-no-capabilities-object"].CanVision, "absent capabilities object must leave CanVision unset")

	// CanVision must flow through to the constructed provider.
	provider := catalog.ProviderFor(byName["claude-opus-4-6"])
	require.True(t, provider.CanVision())
	require.False(t, catalog.ProviderFor(byName["claude-haiku-text-only"]).CanVision())
}
