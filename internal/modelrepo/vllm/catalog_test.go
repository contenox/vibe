package vllm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestCatalogProvider_ListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":            "qwen3:32b",
					"max_model_len": 32768,
				},
			},
		})
	}))
	defer server.Close()

	catalog, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "vllm",
		BaseURL: server.URL,
	})
	require.NoError(t, err)

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)

	model := models[0]
	require.Equal(t, "qwen3:32b", model.Name)
	require.Equal(t, 32768, model.ContextLength)
	require.True(t, model.CanChat)
	require.True(t, model.CanPrompt)
	require.True(t, model.CanStream)
	require.False(t, model.CanEmbed)

	provider := catalog.ProviderFor(model)
	require.Equal(t, "vllm", provider.GetType())
	require.Equal(t, "qwen3:32b", provider.ModelName())
}
