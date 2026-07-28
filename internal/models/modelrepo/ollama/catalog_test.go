package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_CatalogProvider_ListModels(t *testing.T) {
	tagsHit := false
	showHit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			tagsHit = true
			require.Equal(t, http.MethodGet, r.Method)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{
						"name":        "qwen3:8b",
						"model":       "qwen3:8b",
						"modified_at": time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC),
						"size":        12345,
						"digest":      "sha256:test",
						"details":     map[string]any{},
					},
				},
			})
		case "/api/show":
			showHit = true
			require.Equal(t, http.MethodPost, r.Method)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"capabilities":["completion","embedding"],"model_info":{"llama.context_length":4096}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "ollama",
		BaseURL: server.URL,
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.True(t, tagsHit)
	require.True(t, showHit)
	require.Len(t, models, 1)

	model := models[0]
	require.Equal(t, "qwen3:8b", model.Name)
	require.Equal(t, 4096, model.ContextLength)
	require.True(t, model.CanChat)
	require.True(t, model.CanPrompt)
	require.True(t, model.CanStream)
	require.True(t, model.CanEmbed)
	require.False(t, model.CanThink, "Ollama show metadata must not infer thinking from qwen3 model names")
	require.Equal(t, int64(12345), model.Size)
	require.Equal(t, "sha256:test", model.Digest)

	provider := catalog.ProviderFor(model)
	require.Equal(t, "ollama", provider.GetType())
	require.Equal(t, "qwen3:8b", provider.ModelName())
	require.True(t, provider.CanEmbed())
	require.False(t, provider.CanThink())
	require.False(t, model.CanVision, "non-vision model must not claim vision")
}

// CanVision is read from /api/show capabilities, not inferred from the name.
func TestUnit_CatalogProvider_DetectsVisionFromShowCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "llava:7b", "model": "llava:7b", "details": map[string]any{}}},
			})
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"capabilities":["completion","vision"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "ollama", BaseURL: server.URL})
	require.NoError(t, err)
	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.True(t, models[0].CanVision, "vision capability from /api/show must set CanVision")
	require.True(t, catalog.ProviderFor(models[0]).CanVision())
}

// CanThink is read from /api/show capabilities, not inferred from the name.
func TestUnit_CatalogProvider_DetectsThinkingFromShowCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"name": "qwen3:8b", "model": "qwen3:8b", "details": map[string]any{}}},
			})
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"capabilities":["completion","thinking"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "ollama", BaseURL: server.URL})
	require.NoError(t, err)
	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.True(t, models[0].CanThink, "thinking capability from /api/show must set CanThink")
	require.True(t, catalog.ProviderFor(models[0]).CanThink())
}
