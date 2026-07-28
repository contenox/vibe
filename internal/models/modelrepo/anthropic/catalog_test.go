package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// CanVision must derive solely from capabilities.image_input.supported, never from the model name.
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

// Context window comes from max_input_tokens, output ceiling from max_tokens (there is no max_output_tokens field); pagination uses after_id/has_more/last_id.
func TestUnit_CatalogProvider_DocumentedListModelsShape(t *testing.T) {
	const page1 = `{
		"data": [
			{
				"id": "claude-opus-4-6",
				"type": "model",
				"display_name": "Claude Opus 4.6",
				"created_at": "2026-02-04T00:00:00Z",
				"max_input_tokens": 1000000,
				"max_tokens": 128000,
				"capabilities": {
					"batch": {"supported": true},
					"citations": {"supported": true},
					"code_execution": {"supported": true},
					"context_management": {
						"clear_thinking_20251015": {"supported": true},
						"clear_tool_uses_20250919": {"supported": true},
						"compact_20260112": {"supported": true},
						"supported": true
					},
					"effort": {
						"high": {"supported": true},
						"low": {"supported": true},
						"max": {"supported": true},
						"medium": {"supported": true},
						"supported": true,
						"xhigh": {"supported": true}
					},
					"image_input": {"supported": true},
					"pdf_input": {"supported": true},
					"structured_outputs": {"supported": true},
					"thinking": {
						"supported": false,
						"types": {
							"adaptive": {"supported": true},
							"enabled": {"supported": false}
						}
					}
				}
			}
		],
		"first_id": "claude-opus-4-6",
		"has_more": true,
		"last_id": "claude-opus-4-6"
	}`
	const page2 = `{
		"data": [
			{
				"id": "claude-haiku-4-5",
				"type": "model",
				"display_name": "Claude Haiku 4.5",
				"created_at": "2025-10-01T00:00:00Z",
				"max_input_tokens": 200000,
				"max_tokens": 64000,
				"capabilities": {
					"effort": {"supported": true, "high": {"supported": true}},
					"image_input": {"supported": true},
					"thinking": {"supported": false, "types": {"adaptive": {"supported": false}, "enabled": {"supported": false}}}
				}
			}
		],
		"first_id": "claude-haiku-4-5",
		"has_more": false,
		"last_id": "claude-haiku-4-5"
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "claude-opus-4-6" {
			_, _ = w.Write([]byte(page2))
			return
		}
		require.Empty(t, r.URL.Query().Get("after_id"))
		_, _ = w.Write([]byte(page1))
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
	require.Len(t, models, 2, "pagination must follow has_more/last_id")

	byName := map[string]modelrepo.ObservedModel{}
	for _, m := range models {
		byName[m.Name] = m
	}

	opus := byName["claude-opus-4-6"]
	require.Equal(t, 1000000, opus.ContextLength, "context window comes from max_input_tokens")
	require.Equal(t, 128000, opus.MaxOutputTokens, "output ceiling comes from max_tokens")
	require.True(t, opus.CanVision)
	require.True(t, opus.CanThink, "thinking.types.adaptive.supported must count as thinking support")
	require.True(t, opus.CanChat)
	require.True(t, opus.CanStream)

	haiku := byName["claude-haiku-4-5"]
	require.Equal(t, 200000, haiku.ContextLength)
	require.Equal(t, 64000, haiku.MaxOutputTokens)
	require.True(t, haiku.CanVision)
	require.True(t, haiku.CanThink, "effort.supported must count as thinking support")
}
