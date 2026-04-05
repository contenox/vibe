package ollama

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/ollama/ollama/api"
)

type OllamaEmbedClient struct {
	ollamaClient *ollamaHTTPClient
	modelName    string
	backendURL   string
	tracker      libtracker.ActivityTracker
}

func (c *OllamaEmbedClient) Embed(ctx context.Context, text string) ([]float64, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "embed", "ollama", "model", c.modelName)
	defer end()

	resp, err := c.ollamaClient.Embed(ctx, &api.EmbedRequest{
		Model: c.modelName,
		Input: text,
	})
	if err != nil {
		reportErr(err)
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		err := fmt.Errorf("embedding response was empty for model %s", c.modelName)
		reportErr(err)
		return nil, err
	}

	embedding := make([]float64, 0, len(resp.Embeddings[0]))
	for _, v := range resp.Embeddings[0] {
		embedding = append(embedding, float64(v))
	}

	reportChange("embedding_completed", map[string]any{
		"embedding_length": len(embedding),
	})
	return embedding, nil
}

var _ modelrepo.LLMEmbedClient = (*OllamaEmbedClient)(nil)
