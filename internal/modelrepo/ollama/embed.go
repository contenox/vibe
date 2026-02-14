package ollama

import (
	"context"
	"fmt"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
	"github.com/ollama/ollama/api"
)

type OllamaEmbedClient struct {
	ollamaClient *api.Client
	modelName    string
	backendURL   string
	tracker      libtracker.ActivityTracker
}

func (c *OllamaEmbedClient) Embed(ctx context.Context, text string) ([]float64, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "embed", "ollama", "model", c.modelName)
	defer end()

	req := &api.EmbeddingRequest{
		Model:  c.modelName,
		Prompt: text,
	}

	resp, err := c.ollamaClient.Embeddings(ctx, req)
	if err != nil {
		reportErr(err)
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}

	reportChange("embedding_completed", map[string]any{
		"embedding_length": len(resp.Embedding),
	})
	return resp.Embedding, nil
}

var _ modelrepo.LLMEmbedClient = (*OllamaEmbedClient)(nil)
