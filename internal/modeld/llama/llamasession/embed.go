//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
)

const (
	defaultEmbedTokenLimit = 4096
	maxEmbedTokenLimit     = 32768
)

func init() { llama.SetEmbedFunc(embed) }

type embedModel struct {
	model *llamacppshim.Model
	mu    sync.Mutex
}

var embedModels sync.Map // modelPath -> *embedModel

func acquireEmbedModel(modelPath string, cfg llama.Config) (*embedModel, error) {
	if v, ok := embedModels.Load(modelPath); ok {
		return v.(*embedModel), nil
	}
	m, err := llamacppshim.LoadModel(modelPath, modelConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("llama embed: load model %q: %w", modelPath, err)
	}
	em := &embedModel{model: m}
	actual, _ := embedModels.LoadOrStore(modelPath, em)
	return actual.(*embedModel), nil
}

func embedTokenLimit(contextLength int) int {
	limit := contextLength
	if limit <= 0 {
		limit = defaultEmbedTokenLimit
	}
	if limit > maxEmbedTokenLimit {
		limit = maxEmbedTokenLimit
	}
	return limit
}

func embed(ctx context.Context, modelPath string, cfg llama.Config, input string) ([]float64, error) {
	em, err := acquireEmbedModel(modelPath, cfg)
	if err != nil {
		return nil, err
	}
	em.mu.Lock()
	defer em.mu.Unlock()

	tokens, err := em.model.Tokenize(input, true, true)
	if err != nil {
		return nil, fmt.Errorf("llama embed: tokenize: %w", err)
	}
	limit := embedTokenLimit(cfg.NumCtx)
	if len(tokens) > limit {
		return nil, fmt.Errorf("llama embed: input is %d tokens but the limit is %d; raise context length or chunk the input", len(tokens), limit)
	}
	batchSize := len(tokens)
	if batchSize < 1 {
		batchSize = 1
	}

	// Non-causal embedding: the whole input must fit one ubatch.
	lctx, err := llamacppshim.NewContext(em.model, llamacppshim.ContextConfig{
		NumCtx:       batchSize,
		NumBatch:     batchSize,
		NumSeqMax:    1,
		NumThreads:   runtime.NumCPU(),
		Embeddings:   true,
		PoolingLast:  true,
		NonCausalAtt: true,
	})
	if err != nil {
		return nil, fmt.Errorf("llama embed: create context: %w", err)
	}
	defer lctx.Close()
	batch, err := llamacppshim.NewBatch(batchSize, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("llama embed: create batch: %w", err)
	}
	defer batch.Free()

	for i, tok := range tokens {
		if err := batch.Add(tok, nil, i, true, 0); err != nil {
			return nil, fmt.Errorf("llama embed: batch add: %w", err)
		}
	}
	if res := lctx.Decode(batch); res.Status != llamacppshim.DecodeOK {
		return nil, fmt.Errorf("llama embed: decode: %w", res.Err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	emb, err := extractEmbedding(lctx, len(tokens))
	if err != nil {
		return nil, err
	}
	return l2Normalize(emb)
}

func extractEmbedding(lctx *llamacppshim.Context, numTokens int) ([]float64, error) {
	if pooled := lctx.GetEmbeddingsSeq(0); pooled != nil {
		out := make([]float64, len(pooled))
		for i, v := range pooled {
			out[i] = float64(v)
		}
		return out, nil
	}
	var sum []float64
	n := 0
	for i := 0; i < numTokens; i++ {
		tok := lctx.GetEmbeddingsIth(i)
		if tok == nil {
			continue
		}
		if sum == nil {
			sum = make([]float64, len(tok))
		}
		for j, v := range tok {
			sum[j] += float64(v)
		}
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("llama embed: no embeddings returned (model may not support embedding extraction)")
	}
	for j := range sum {
		sum[j] /= float64(n)
	}
	return sum, nil
}

func l2Normalize(vec []float64) ([]float64, error) {
	var sum float64
	for _, v := range vec {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("llama embed: embedding contains NaN or Inf")
		}
		sum += v * v
	}
	norm := 1.0 / math.Max(math.Sqrt(sum), 1e-12)
	for i := range vec {
		vec[i] *= norm
	}
	return vec, nil
}
