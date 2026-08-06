package modelrepo_test

import (
	"net/http"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/ollama"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

// thinkModel is the smallest checkpoint that reports the "thinking"
// capability (~500MB).
const thinkModel = "qwen3:0.6b"

// TestSystem_Ollama_Thinking exercises the reasoning path against a live
// server. ThinkValue marshals as a bool for "off" and a string for a named
// level, and a real server rejects the wrong shape outright — which fakes
// cannot do.
func TestSystem_Ollama_Thinking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ollama thinking system test: starts a container and pulls a reasoning model")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := t.Context()
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, "latest")
	require.NoError(t, err)
	defer cleanup()

	t.Logf("Pulling reasoning model: %s", thinkModel)
	require.NoError(t, pullModel(t, uri, thinkModel))
	require.NoError(t, waitForModelReady(t, uri, thinkModel))

	newChatClient := func(t *testing.T) modelrepo.LLMChatClient {
		t.Helper()
		caps := modelrepo.CapabilityConfig{
			ContextLength: 4096,
			CanChat:       true,
			CanThink:      true,
		}
		provider := ollama.NewOllamaProvider(thinkModel, []string{uri}, http.DefaultClient, caps, "", nil)
		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)
		return chatClient
	}

	t.Run("CapabilityDerivation", func(t *testing.T) {
		catalog, err := modelrepo.NewCatalogProvider(
			modelrepo.BackendSpec{Type: "ollama", BaseURL: uri},
			modelrepo.WithCatalogHTTPClient(http.DefaultClient),
		)
		require.NoError(t, err)

		observed, err := catalog.ListModels(ctx)
		require.NoError(t, err)

		var found bool
		for _, m := range observed {
			if m.Name != thinkModel {
				continue
			}
			found = true
			assert.True(t, m.CanThink, "thinking capability not derived for %s", thinkModel)
			assert.True(t, m.CanChat, "completion capability not derived for %s", thinkModel)
		}
		require.True(t, found, "listed models do not include %s", thinkModel)
	})

	// A named level marshals as the JSON string "low".
	t.Run("ThinkLevelIsString", func(t *testing.T) {
		resp, err := newChatClient(t).Chat(ctx,
			[]modelrepo.Message{{Role: "user", Content: "What is 2 + 2?"}},
			modelrepo.WithThink("low"),
			modelrepo.WithMaxTokens(512))
		require.NoError(t, err, "server rejected think as a string level")
		assert.NotEmpty(t, resp.Message.Thinking, "no reasoning trace returned for think=\"low\"")
	})

	// "off" marshals as the JSON bool false, a different wire shape from the
	// level strings above.
	t.Run("ThinkOffIsBool", func(t *testing.T) {
		resp, err := newChatClient(t).Chat(ctx,
			[]modelrepo.Message{{Role: "user", Content: "What is 2 + 2?"}},
			modelrepo.WithThink("off"),
			modelrepo.WithMaxTokens(256))
		require.NoError(t, err, "server rejected think as a bool")
		assert.Empty(t, resp.Message.Thinking, "reasoning trace returned despite think=false")
	})
}
