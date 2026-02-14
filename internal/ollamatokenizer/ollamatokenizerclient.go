package ollamatokenizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/vibe/libtracker"
)

// HTTPClient implements the Tokenizer interface using HTTP calls to the tokenizer service.
type HTTPClient struct {
	baseURL string
	client  *http.Client
}

// ConfigHTTP contains configuration for the HTTP client.
type ConfigHTTP struct {
	BaseURL string
}

// NewHTTPClient creates a new HTTP-based tokenizer client.
func NewHTTPClient(ctx context.Context, cfg ConfigHTTP) (Tokenizer, func() error, error) {
	cleanup := func() error { return nil }
	client := &HTTPClient{
		baseURL: cfg.BaseURL,
		client:  http.DefaultClient,
	}
	return client, cleanup, nil
}

// ping checks if the tokenizer service is healthy.
func (c *HTTPClient) ping(ctx context.Context) error {
	healthURL := fmt.Sprintf("%s/healthz", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed with status %d", resp.StatusCode)
	}
	return nil
}

// -------- /tokenize types --------

type tokenizeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type tokenizeResponse struct {
	Tokens []int `json:"tokens"`
	Count  int   `json:"count"`
}

// -------- /count types --------

type countRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type countResponse struct {
	Count int `json:"count"`
}

// Tokenize sends a tokenization request to the HTTP service.
func (c *HTTPClient) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	reqBody := tokenizeRequest{
		Model:  modelName,
		Prompt: prompt,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	tokenizeURL := fmt.Sprintf("%s/tokenize", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", tokenizeURL, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("model '%s' not found", modelName)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("tokenize failed with status %d: %s", resp.StatusCode, string(body))
	}

	var response tokenizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return response.Tokens, nil
}

// CountTokens uses the dedicated /count endpoint, with a backward-compatible fallback to /tokenize.
func (c *HTTPClient) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	reqBody := countRequest{
		Model:  modelName,
		Prompt: prompt,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	countURL := fmt.Sprintf("%s/count", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", countURL, bytes.NewReader(reqJSON))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("count failed with status %d: %s", resp.StatusCode, string(body))
	}

	var response countResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return response.Count, nil
}

// OptimalModel returns the optimal model for tokenization based on the given model.
// This is a client-side implementation mirroring the server's logic.
func (c *HTTPClient) OptimalModel(ctx context.Context, baseModel string) (string, error) {
	// Hardcoded model mappings (matching what the server uses with USE_DEFAULT_URLS=true)
	modelURLs := map[string]string{
		"tiny":                  "https://huggingface.co/Hjgugugjhuhjggg/FastThink-0.5B-Tiny-Q2_K-GGUF/resolve/main/fastthink-0.5b-tiny-q2_k.gguf",
		"llama-3.1":             "https://huggingface.co/bartowski/Meta-Llama-3.1-8B-Instruct-GGUF/resolve/main/Meta-Llama-3.1-8B-Instruct-IQ2_M.gguf",
		"llama-3.2":             "https://huggingface.co/unsloth/Llama-3.2-3B-Instruct-GGUF/blob/main/Llama-3.2-3B-Instruct-Q2_K.gguf",
		"granite-embedding-30m": "https://huggingface.co/bartowski/granite-embedding-30m-english-GGUF/resolve/main/granite-embedding-30m-english-f16.gguf",
		"phi-3":                 "https://huggingface.co/microsoft/Phi-3-mini-4k-instruct-gguf/resolve/main/Phi-3-mini-4k-instruct-q4.gguf",
	}

	// Hardcoded family mappings (matching the server's logic)
	familyMappings := []struct {
		CanonicalName string
		Substrings    []string
	}{
		{CanonicalName: "llama-3.2", Substrings: []string{"llama-3.2", "llama3.2"}},
		{CanonicalName: "llama-3.1", Substrings: []string{"llama-3.1", "llama3.1", "llama-3", "llama3"}},
		{CanonicalName: "phi-3", Substrings: []string{"phi-3", "phi3"}},
	}

	fallback := "granite-embedding-30m" // From docker-compose environment

	baseModel = strings.ToLower(baseModel)
	baseModel = strings.Split(baseModel, ":")[0]

	if _, exists := modelURLs[baseModel]; exists {
		return baseModel, nil
	}

	for _, mapping := range familyMappings {
		if _, canonicalExists := modelURLs[mapping.CanonicalName]; !canonicalExists {
			continue
		}

		for _, sub := range mapping.Substrings {
			if strings.Contains(baseModel, sub) {
				return mapping.CanonicalName, nil
			}
		}
	}

	return fallback, nil
}

type activityTrackerDecorator struct {
	client  Tokenizer
	tracker libtracker.ActivityTracker
}

type Tokenizer interface {
	Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error)
	CountTokens(ctx context.Context, modelName string, prompt string) (int, error)
	OptimalModel(ctx context.Context, baseModel string) (string, error)
}

func (d *activityTrackerDecorator) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"tokenize",
		"tokenizer",
		"model", modelName,
		"prompt_length", len(prompt),
	)
	defer endFn()

	tokens, err := d.client.Tokenize(ctx, modelName, prompt)
	if err != nil {
		reportErrFn(err)
	}

	return tokens, err
}

func (d *activityTrackerDecorator) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"count",
		"tokenizer",
		"model", modelName,
		"prompt_length", len(prompt),
	)
	defer endFn()

	count, err := d.client.CountTokens(ctx, modelName, prompt)
	if err != nil {
		reportErrFn(err)
	}

	return count, err
}

func (d *activityTrackerDecorator) OptimalModel(ctx context.Context, baseModel string) (string, error) {
	reportErrFn, _, endFn := d.tracker.Start(
		ctx,
		"optimal_model",
		"tokenizer",
		"base_model", baseModel,
	)
	defer endFn()

	model, err := d.client.OptimalModel(ctx, baseModel)
	if err != nil {
		reportErrFn(err)
	}

	return model, err
}

// WithActivityTracker decorates the given Tokenizer with activity tracking
func WithActivityTracker(client Tokenizer, tracker libtracker.ActivityTracker) Tokenizer {
	return &activityTrackerDecorator{
		client:  client,
		tracker: tracker,
	}
}

var _ Tokenizer = (*activityTrackerDecorator)(nil)
