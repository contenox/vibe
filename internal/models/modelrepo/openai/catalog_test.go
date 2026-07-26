package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_CatalogProvider_PaginatesListModels(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/models", r.URL.Path)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":     []map[string]any{{"id": "gpt-5"}},
				"has_more": true,
				"last_id":  "gpt-5",
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":     []map[string]any{{"id": "gpt-4o"}},
				"has_more": false,
			})
		}
	}))
	defer srv.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "openai", BaseURL: srv.URL, APIKey: "k"})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, calls, "must have fetched two pages")
	require.Len(t, models, 2)
	names := []string{models[0].Name, models[1].Name}
	require.Contains(t, names, "gpt-5")
	require.Contains(t, names, "gpt-4o")
}

func TestUnit_CatalogProvider_ListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/models", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "gpt-5"},
				{"id": "text-embedding-3-small"},
			},
		})
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "openai",
		BaseURL: server.URL,
		APIKey:  "test-key",
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 2)

	require.Equal(t, "gpt-5", models[0].Name)
	require.True(t, models[0].CanChat)
	require.True(t, models[0].CanPrompt)
	require.True(t, models[0].CanStream)
	require.False(t, models[0].CanEmbed)
	require.False(t, models[0].CanThink, "OpenAI /models does not expose reasoning capability metadata")
	require.True(t, models[0].CanVision, "gpt-5 supports image input (maintained list)")
	require.Equal(t, 128000, models[0].MaxOutputTokens)

	require.Equal(t, "text-embedding-3-small", models[1].Name)
	require.False(t, models[1].CanChat)
	require.True(t, models[1].CanEmbed)
	require.False(t, models[1].CanVision, "embedding model has no image input")
	require.Equal(t, 0, models[1].MaxOutputTokens)

	provider := catalog.ProviderFor(models[0])
	require.Equal(t, "openai", provider.GetType())
	require.Equal(t, "gpt-5", provider.ModelName())
	require.False(t, provider.CanThink())
	require.True(t, provider.CanVision(), "gpt-5 vision flows through to the provider")
}

// TestUnit_CatalogProvider_VisionLandmines verifies the maintained OpenAI vision
// list flows through ListModels for the tricky splits: base gpt-4 and the *-mini
// reasoning models are text-only, gpt-4o/o4-mini have vision, and audio models
// (which are not chat) never claim it.
func TestUnit_CatalogProvider_VisionLandmines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "gpt-4o"},
				{"id": "gpt-4"},
				{"id": "o3-mini"},
				{"id": "o4-mini"},
				{"id": "gpt-4o-audio-preview"},
			},
		})
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{Type: "openai", BaseURL: server.URL, APIKey: "k"})
	require.NoError(t, err)
	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)

	vision := map[string]bool{}
	for _, m := range models {
		vision[m.Name] = m.CanVision
	}
	require.True(t, vision["gpt-4o"], "gpt-4o has vision")
	require.False(t, vision["gpt-4"], "base gpt-4 is text-only")
	require.False(t, vision["o3-mini"], "o3-mini is text-only")
	require.True(t, vision["o4-mini"], "o4-mini has vision")
	require.False(t, vision["gpt-4o-audio-preview"], "audio model is not chat-vision")
}
