package local

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/ollama/ollama/llama"
	"github.com/ollama/ollama/ml"
)

const (
	defaultNumCtx   = 4096
	defaultBatch    = 512
	defaultMaxTokens = 2048
)

// localChatClient implements modelrepo.LLMChatClient using llama.cpp in-process.
type localChatClient struct {
	modelPath string
}

func (c *localChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}

	prompt := buildPrompt(messages)
	text, err := generate(ctx, c.modelPath, prompt, cfg)
	if err != nil {
		return modelrepo.ChatResult{}, err
	}
	return modelrepo.ChatResult{
		Message: modelrepo.Message{Role: "assistant", Content: text},
	}, nil
}

// localPromptClient implements modelrepo.LLMPromptExecClient.
type localPromptClient struct {
	modelPath string
}

func (c *localPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	messages := []modelrepo.Message{
		{Role: "system", Content: systemInstruction},
		{Role: "user", Content: prompt},
	}
	temp := float64(temperature)
	cfg := &modelrepo.ChatConfig{Temperature: &temp}
	return generate(ctx, c.modelPath, buildPrompt(messages), cfg)
}

// buildPrompt converts messages to a simple chat-ML format.
// Models with a bundled chat template will re-tokenize correctly;
// for models without one this provides a reasonable fallback.
func buildPrompt(messages []modelrepo.Message) string {
	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			fmt.Fprintf(&b, "<|system|>\n%s\n", m.Content)
		case "user":
			fmt.Fprintf(&b, "<|user|>\n%s\n", m.Content)
		case "assistant":
			fmt.Fprintf(&b, "<|assistant|>\n%s\n", m.Content)
		default:
			fmt.Fprintf(&b, "%s\n", m.Content)
		}
	}
	b.WriteString("<|assistant|>\n")
	return b.String()
}

func generate(ctx context.Context, modelPath, prompt string, cfg *modelrepo.ChatConfig) (string, error) {
	lm, err := acquireModel(modelPath)
	if err != nil {
		return "", err
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	ctxParams := llama.NewContextParams(
		defaultNumCtx,
		defaultBatch,
		1,
		runtime.NumCPU(),
		ml.FlashAttentionDisabled,
		"",
	)
	llamaCtx, err := llama.NewContextWithModel(lm.model, ctxParams)
	if err != nil {
		return "", fmt.Errorf("create context: %w", err)
	}

	tokens, err := lm.model.Tokenize(prompt, true, true)
	if err != nil {
		return "", fmt.Errorf("tokenize: %w", err)
	}

	batch, err := llama.NewBatch(defaultBatch, 0, 0)
	if err != nil {
		return "", fmt.Errorf("create batch: %w", err)
	}
	defer batch.Free()

	for i, tok := range tokens {
		batch.Add(tok, nil, i, i == len(tokens)-1, 0)
	}
	if err := llamaCtx.Decode(batch); err != nil {
		return "", fmt.Errorf("decode prompt: %w", err)
	}

	samplerParams := llama.SamplingParams{
		TopK:  40,
		TopP:  0.9,
		MinP:  0.05,
		Temp:  0.8,
	}
	if cfg != nil && cfg.Temperature != nil {
		samplerParams.Temp = float32(*cfg.Temperature)
	}

	sampler, err := llama.NewSamplingContext(lm.model, samplerParams)
	if err != nil {
		return "", fmt.Errorf("create sampler: %w", err)
	}

	maxTokens := defaultMaxTokens
	if cfg != nil && cfg.MaxTokens != nil && *cfg.MaxTokens > 0 {
		maxTokens = *cfg.MaxTokens
	}

	var out strings.Builder
	for pos := len(tokens); pos < len(tokens)+maxTokens; pos++ {
		select {
		case <-ctx.Done():
			return out.String(), ctx.Err()
		default:
		}

		id := sampler.Sample(llamaCtx, -1)
		sampler.Accept(id, true)

		if lm.model.TokenIsEog(id) {
			break
		}

		out.WriteString(lm.model.TokenToPiece(id))

		batch.Clear()
		batch.Add(id, nil, pos, true, 0)
		if err := llamaCtx.Decode(batch); err != nil {
			return out.String(), fmt.Errorf("decode token: %w", err)
		}
	}

	return strings.TrimSpace(out.String()), nil
}

// localStreamClient implements modelrepo.LLMStreamClient using llama.cpp in-process.
type localStreamClient struct {
	modelPath string
}

func (c *localStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}

	prompt := buildPrompt(messages)
	ch := make(chan *modelrepo.StreamParcel, 16)

	go func() {
		defer close(ch)

		lm, err := acquireModel(c.modelPath)
		if err != nil {
			ch <- &modelrepo.StreamParcel{Error: err}
			return
		}
		lm.mu.Lock()
		defer lm.mu.Unlock()

		ctxParams := llama.NewContextParams(defaultNumCtx, defaultBatch, 1, runtime.NumCPU(), ml.FlashAttentionDisabled, "")
		llamaCtx, err := llama.NewContextWithModel(lm.model, ctxParams)
		if err != nil {
			ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("create context: %w", err)}
			return
		}

		tokens, err := lm.model.Tokenize(prompt, true, true)
		if err != nil {
			ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("tokenize: %w", err)}
			return
		}

		batch, err := llama.NewBatch(defaultBatch, 0, 0)
		if err != nil {
			ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("create batch: %w", err)}
			return
		}
		defer batch.Free()

		for i, tok := range tokens {
			batch.Add(tok, nil, i, i == len(tokens)-1, 0)
		}
		if err := llamaCtx.Decode(batch); err != nil {
			ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("decode prompt: %w", err)}
			return
		}

		samplerParams := llama.SamplingParams{TopK: 40, TopP: 0.9, MinP: 0.05, Temp: 0.8}
		if cfg.Temperature != nil {
			samplerParams.Temp = float32(*cfg.Temperature)
		}
		sampler, err := llama.NewSamplingContext(lm.model, samplerParams)
		if err != nil {
			ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("create sampler: %w", err)}
			return
		}

		maxTokens := defaultMaxTokens
		if cfg.MaxTokens != nil && *cfg.MaxTokens > 0 {
			maxTokens = *cfg.MaxTokens
		}

		for pos := len(tokens); pos < len(tokens)+maxTokens; pos++ {
			select {
			case <-ctx.Done():
				ch <- &modelrepo.StreamParcel{Error: ctx.Err()}
				return
			default:
			}
			id := sampler.Sample(llamaCtx, -1)
			sampler.Accept(id, true)
			if lm.model.TokenIsEog(id) {
				break
			}
			ch <- &modelrepo.StreamParcel{Data: lm.model.TokenToPiece(id)}
			batch.Clear()
			batch.Add(id, nil, pos, true, 0)
			if err := llamaCtx.Decode(batch); err != nil {
				ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("decode token: %w", err)}
				return
			}
		}
	}()
	return ch, nil
}

// localEmbedClient implements modelrepo.LLMEmbedClient using llama.cpp in-process.
type localEmbedClient struct {
	modelPath string
}

func (c *localEmbedClient) Embed(ctx context.Context, prompt string) ([]float64, error) {
	lm, err := acquireModel(c.modelPath)
	if err != nil {
		return nil, err
	}
	lm.mu.Lock()
	defer lm.mu.Unlock()

	ctxParams := llama.NewContextParams(defaultNumCtx, defaultBatch, 1, runtime.NumCPU(), ml.FlashAttentionDisabled, "")
	llamaCtx, err := llama.NewContextWithModel(lm.model, ctxParams)
	if err != nil {
		return nil, fmt.Errorf("create context: %w", err)
	}

	tokens, err := lm.model.Tokenize(prompt, true, true)
	if err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}

	batch, err := llama.NewBatch(defaultBatch, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("create batch: %w", err)
	}
	defer batch.Free()

	for i, tok := range tokens {
		batch.Add(tok, nil, i, true, 0)
	}
	if err := llamaCtx.Decode(batch); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	emb32 := llamaCtx.GetEmbeddingsSeq(0)
	if emb32 == nil {
		return nil, fmt.Errorf("no embeddings returned (model may not support embedding extraction)")
	}
	emb64 := make([]float64, len(emb32))
	for i, v := range emb32 {
		emb64[i] = float64(v)
	}
	return emb64, nil
}
