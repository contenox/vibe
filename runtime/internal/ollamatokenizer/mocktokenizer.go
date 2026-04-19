package ollamatokenizer

import (
	"context"
	"fmt"
)

// MockTokenizer is a mock implementation of the Tokenizer interface.
type MockTokenizer struct {
	FixedTokenCount int
	FixedModel      string
	CustomTokens    map[string][]int
}

var _ Tokenizer = (*MockTokenizer)(nil)

func (m MockTokenizer) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	if tokens, ok := m.CustomTokens[prompt]; ok {
		return tokens, nil
	}
	// Just return a slice of dummy token values, e.g., one per word
	wordCount := len(splitWords(prompt))
	tokens := make([]int, wordCount)
	for i := range wordCount {
		tokens[i] = i + 1
	}
	return tokens, nil
}

func (m MockTokenizer) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	if m.FixedTokenCount > 0 {
		return m.FixedTokenCount, nil
	}
	return len(splitWords(prompt)), nil
}

func (m MockTokenizer) OptimalModel(ctx context.Context, baseModel string) (string, error) {
	if m.FixedModel != "" {
		return m.FixedModel, nil
	}
	return fmt.Sprintf("%s-optimized", baseModel), nil
}

// Simple word splitter for fake tokenization
func splitWords(s string) []string {
	// Replace with a more realistic splitter if needed
	var words []string
	start := -1
	for i, c := range s {
		if c == ' ' || c == '\n' || c == '\t' {
			if start != -1 {
				words = append(words, s[start:i])
				start = -1
			}
		} else if start == -1 {
			start = i
		}
	}
	if start != -1 {
		words = append(words, s[start:])
	}
	return words
}
