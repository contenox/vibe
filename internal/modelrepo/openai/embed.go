package openai

import (
	"context"
	"fmt"

	"github.com/contenox/vibe/internal/modelrepo"
)

type OpenAIEmbedClient struct {
	openAIClient
}

type openAIEmbedRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
}

type openAIEmbedResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func (c *OpenAIEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "embed", "openai", "model", c.modelName)
	defer end()

	request := openAIEmbedRequest{
		Model:          c.modelName,
		Input:          prompt,
		EncodingFormat: "float",
	}

	var response openAIEmbedResponse
	if err := c.sendRequest(ctx, "/embeddings", request, &response); err != nil {
		reportErr(err)
		return nil, err
	}

	if len(response.Data) == 0 || len(response.Data[0].Embedding) == 0 {
		err := fmt.Errorf("no embedding data returned from OpenAI for model %s", c.modelName)
		reportErr(err)
		return nil, err
	}

	embedding := response.Data[0].Embedding
	reportChange("embedding_completed", map[string]any{
		"embedding_length": len(embedding),
		"prompt_tokens":    response.Usage.PromptTokens,
		"total_tokens":     response.Usage.TotalTokens,
	})
	return embedding, nil
}

var _ modelrepo.LLMEmbedClient = (*OpenAIEmbedClient)(nil)
