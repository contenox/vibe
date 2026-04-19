package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestCatalogProvider_ListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "test-key", r.Header.Get("X-Goog-Api-Key"))
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v1beta/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{"name": "models/gemini-2.5-flash"},
				},
			})
		case "/v1beta/models/gemini-2.5-flash":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "models/gemini-2.5-flash",
				"inputTokenLimit": 8192,
				"supportedGenerationMethods": []string{
					"generateContent",
					"embedContent",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "gemini",
		BaseURL: server.URL,
		APIKey:  "test-key",
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)

	model := models[0]
	require.Equal(t, "models/gemini-2.5-flash", model.Name)
	require.Equal(t, 8192, model.ContextLength)
	require.True(t, model.CanChat)
	require.True(t, model.CanPrompt)
	require.True(t, model.CanStream)
	require.True(t, model.CanEmbed)

	provider := catalog.ProviderFor(model)
	require.Equal(t, "gemini", provider.GetType())
	require.Equal(t, "gemini-2.5-flash", provider.ModelName())
}
