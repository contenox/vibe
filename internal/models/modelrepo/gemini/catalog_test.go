package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_CatalogProvider_DoesNotInferThinkingFromModelName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "models/gemini-2.5-pro"}},
			})
		case "/v1beta/models/gemini-2.5-pro":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":            "models/gemini-2.5-pro",
				"inputTokenLimit": 200000,
				"supportedGenerationMethods": []string{
					"generateContent",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "gemini", BaseURL: server.URL})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "models/gemini-2.5-pro", models[0].Name)
	require.False(t, models[0].CanThink, "Gemini thinking support must come from the model resource thinking field")
}

func TestUnit_CatalogProvider_ListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "test-key", r.Header.Get("X-Goog-Api-Key"))
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v1beta/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{"name": "models/gemini-flash-latest"},
				},
			})
		case "/v1beta/models/gemini-flash-latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":             "models/gemini-flash-latest",
				"inputTokenLimit":  8192,
				"outputTokenLimit": 2048,
				"thinking":         true,
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
	require.Equal(t, "models/gemini-flash-latest", model.Name)
	require.Equal(t, 8192, model.ContextLength)
	require.Equal(t, 2048, model.MaxOutputTokens)
	require.True(t, model.CanChat)
	require.True(t, model.CanPrompt)
	require.True(t, model.CanStream)
	require.True(t, model.CanEmbed)
	require.True(t, model.CanThink)

	provider := catalog.ProviderFor(model)
	require.Equal(t, "gemini", provider.GetType())
	require.Equal(t, "gemini-flash-latest", provider.ModelName())
	require.True(t, provider.CanThink())
}
