package gemini

import (
	"context"
	"fmt"

	"github.com/contenox/vibe/internal/modelrepo"
)

type GeminiEmbedClient struct {
	geminiClient
}

type geminiEmbedContentRequest struct {
	Model   string        `json:"model"`
	Content geminiContent `json:"content"`
}

type geminiEmbedContentResponse struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

func (c *GeminiEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "embed", "gemini", "model", c.modelName)
	defer end()

	request := geminiEmbedContentRequest{
		Model: "models/" + c.modelName,
		Content: geminiContent{
			Parts: []geminiPart{{Text: prompt}},
		},
	}

	endpoint := fmt.Sprintf("/v1beta/models/%s:embedContent", c.modelName)
	var response geminiEmbedContentResponse
	if err := c.sendRequest(ctx, endpoint, request, &response); err != nil {
		reportErr(err)
		return nil, err
	}

	if len(response.Embedding.Values) == 0 {
		err := fmt.Errorf("no embedding values returned from Gemini for model %s", c.modelName)
		reportErr(err)
		return nil, err
	}

	reportChange("embedding_completed", map[string]any{
		"embedding_length": len(response.Embedding.Values),
	})
	return response.Embedding.Values, nil
}

var _ modelrepo.LLMEmbedClient = (*GeminiEmbedClient)(nil)
