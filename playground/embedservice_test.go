package playground_test

import (
	"testing"

	"github.com/contenox/vibe/playground"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_EmbedService(t *testing.T) {
	ctx := t.Context()

	// Create playground with all required dependencies
	p := playground.New()
	p = p.WithPostgresTestContainer(ctx)
	p = p.WithNats(ctx)
	p = p.WithRuntimeState(ctx, true) // With groups
	p = p.WithMockTokenizer()
	// Configure model names and providers
	p = p.WithInternalOllamaEmbedder(ctx, "nomic-embed-text:latest", 1024)

	// Set up Ollama backend and assign to embedding group
	p = p.WithOllamaBackend(ctx, "embed-backend", "latest", true, false)
	p = p.StartBackgroundRoutines(ctx)
	p = p.WithLLMRepo()
	require.NoError(t, p.GetError(), "Failed to set up Ollama backend")

	// Wait for model to be ready
	err := p.WaitUntilModelIsReady(ctx, "embed-backend", "nomic-embed-text:latest")
	require.NoError(t, err, "Model not ready in time")

	// Get services
	embedService, err := p.GetEmbedService()
	require.NoError(t, err)
	modelService, err := p.GetModelService()
	require.NoError(t, err)

	t.Run("Embedding", func(t *testing.T) {
		models, err := modelService.List(ctx, nil, 10)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(models), 1, "Should have at least one model")

		// Find the embedding model
		var embedModel *runtimetypes.Model
		for _, m := range models {
			if m.Model == "nomic-embed-text:latest" {
				embedModel = m
				break
			}
		}
		require.NotNil(t, embedModel, "Embedding model should exist")
		assert.True(t, embedModel.CanEmbed, "Model should have embedding capability")

		text := "This is a test sentence for embedding."
		embedding, err := embedService.Embed(ctx, text)
		require.NoError(t, err)
		require.NotNil(t, embedding, "Embedding should not be nil")
		assert.Greater(t, len(embedding), 0, "Embedding should have dimensions")

		_, err = embedService.Embed(ctx, "")
		require.Error(t, err)
	})
}
