package ollamatokenizer

import (
	"context"
	"unicode/utf8"
)

// EstimateTokenizer implements Tokenizer using simple character-based estimates.
// Use for local single-process mode where no tokenizer service is available.
// CountTokens uses ~4 chars per token (rough heuristic for typical LLM tokenizers).
type EstimateTokenizer struct{}

var _ Tokenizer = (*EstimateTokenizer)(nil)

// NewEstimateTokenizer returns a tokenizer that estimates token counts without a remote service.
func NewEstimateTokenizer() *EstimateTokenizer {
	return &EstimateTokenizer{}
}

// Tokenize returns a dummy slice of length equal to the estimated token count.
// No caller in the task engine uses the actual token IDs; the length is sufficient.
func (e *EstimateTokenizer) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	n := estimateTokens(prompt)
	tokens := make([]int, n)
	for i := range n {
		tokens[i] = i + 1
	}
	return tokens, nil
}

// CountTokens returns an estimated token count (runes / 4, min 1).
func (e *EstimateTokenizer) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return estimateTokens(prompt), nil
}

// OptimalModel returns the base model unchanged (no proxy tokenizer model).
func (e *EstimateTokenizer) OptimalModel(ctx context.Context, baseModel string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return baseModel, nil
}

func estimateTokens(s string) int {
	r := utf8.RuneCountInString(s)
	if r == 0 {
		return 0
	}
	// Common rough heuristic: ~4 characters per token for English
	n := r / 4
	if n < 1 {
		n = 1
	}
	return n
}
