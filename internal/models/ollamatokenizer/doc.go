// Package ollamatokenizer provides Tokenizer implementations used by llmrepo
// to count and split tokens for a given model.
//
// NewHTTPClient talks to an Ollama-compatible tokenizer endpoint;
// EstimateTokenizer is a dependency-free heuristic fallback; MockTokenizer
// is intended for tests.
package ollamatokenizer
