package vertex

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/models/modelrepo"
)

type vertexEmbedClient struct {
	vertexClient
}

// vertexPredictEmbedRequest is the documented Vertex AI text-embedding request:
// POST .../publishers/{publisher}/models/{model}:predict with one instance per
// input text.
type vertexPredictEmbedRequest struct {
	Instances []vertexEmbedInstance `json:"instances"`
}

type vertexEmbedInstance struct {
	Content string `json:"content"`
}

type vertexPredictEmbedResponse struct {
	Predictions []struct {
		Embeddings struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	} `json:"predictions"`
}

func (c *vertexEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "embed", "vertex", "model", c.modelName)
	defer end()

	request := vertexPredictEmbedRequest{
		Instances: []vertexEmbedInstance{{Content: prompt}},
	}
	var response vertexPredictEmbedResponse
	if err := c.sendRequest(ctx, c.endpoint("predict"), request, &response); err != nil {
		reportErr(err)
		return nil, err
	}

	if len(response.Predictions) == 0 || len(response.Predictions[0].Embeddings.Values) == 0 {
		err := fmt.Errorf("vertex model %s returned no embedding values; it may not be an embedding model — use a text-embedding model such as gemini-embedding-001", c.modelName)
		reportErr(err)
		return nil, err
	}

	reportChange("embedding_completed", map[string]any{
		"embedding_length": len(response.Predictions[0].Embeddings.Values),
	})
	return response.Predictions[0].Embeddings.Values, nil
}

var _ modelrepo.LLMEmbedClient = (*vertexEmbedClient)(nil)
