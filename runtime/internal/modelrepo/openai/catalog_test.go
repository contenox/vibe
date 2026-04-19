package openai

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

	require.Equal(t, "text-embedding-3-small", models[1].Name)
	require.False(t, models[1].CanChat)
	require.True(t, models[1].CanEmbed)

	provider := catalog.ProviderFor(models[0])
	require.Equal(t, "openai", provider.GetType())
	require.Equal(t, "gpt-5", provider.ModelName())
}
