package workspaceindex

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/models/llmrepo"
)

// EmbedSeam is the one method the index needs from llmrepo's model manager.
// *llmrepo.modelManager satisfies it via llmrepo.ModelRepo.
type EmbedSeam interface {
	Embed(ctx context.Context, embedReq llmrepo.EmbedRequest, prompt string) ([]float64, llmrepo.Meta, error)
}

// llmRepoEmbedder adapts llmrepo's Embed to the package's narrow Embedder.
type llmRepoEmbedder struct {
	seam     EmbedSeam
	model    string
	provider string
}

// NewLLMRepoEmbedder wires the index to the runtime's real embedding path:
// the provider-agnostic Embed that already resolves through the model
// router. model/provider are passed explicitly so the model recorded on an
// index config is provably the one that produced its vectors. The
// float64-to-float32 conversion is deliberate: no embedding model has
// meaningful precision past float32, and it halves the stored blob.
func NewLLMRepoEmbedder(seam EmbedSeam, model, provider string) Embedder {
	return &llmRepoEmbedder{seam: seam, model: model, provider: provider}
}

func (e *llmRepoEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	raw, _, err := e.seam.Embed(ctx, llmrepo.EmbedRequest{
		ModelName:    e.model,
		ProviderType: e.provider,
	}, text)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("embedding model %q returned no vector", e.model)
	}
	out := make([]float32, len(raw))
	for i, v := range raw {
		out[i] = float32(v)
	}
	return out, nil
}
