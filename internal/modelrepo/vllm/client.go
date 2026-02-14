package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

// vLLMPromptClient handles prompt execution
type vLLMPromptClient struct {
	vLLMClient
}

// vLLMChatClient handles chat interaction
type vLLMChatClient struct {
	vLLMClient
}

type vLLMClient struct {
	baseURL    string
	httpClient *http.Client
	modelName  string
	maxTokens  int
	apiKey     string
	tracker    libtracker.ActivityTracker
}

type chatRequest struct {
	Model       string              `json:"model"`
	Messages    []modelrepo.Message `json:"messages"`
	Temperature *float64            `json:"temperature,omitempty"`
	MaxTokens   *int                `json:"max_tokens,omitempty"`
	TopP        *float64            `json:"top_p,omitempty"`
	Seed        *int                `json:"seed,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	Tools       []modelrepo.Tool    `json:"tools,omitempty"`
}

func (c *vLLMClient) sendRequest(ctx context.Context, endpoint string, request interface{}, response interface{}) error {
	url := c.baseURL + endpoint
	reqBody, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	tracker := c.tracker
	reportErr, reportChange, end := tracker.Start(
		ctx,
		"http_request",
		"vllm",
		"model", c.modelName,
		"endpoint", endpoint,
		"base_url", c.baseURL,
	)
	defer end()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		err = fmt.Errorf("failed to create request: %w", err)
		reportErr(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		err = fmt.Errorf("HTTP request failed for model %s: %w", c.modelName, err)
		reportErr(err)
		return err
	}
	defer resp.Body.Close()

	// Log headers
	reportChange("http_response", map[string]any{
		"status_code": resp.StatusCode,
		"headers":     resp.Header,
	})

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("vLLM API returned non-200 status: %d, body: %s for model %s", resp.StatusCode, string(bodyBytes), c.modelName)
		reportErr(err)
		return err
	}

	if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
		err = fmt.Errorf("failed to decode response for model %s: %w", c.modelName, err)
		reportErr(err)
		return err
	}

	reportChange("request_completed", nil)
	return nil
}

func buildChatRequest(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument) chatRequest {
	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	return chatRequest{
		Model:       modelName,
		Messages:    messages,
		Temperature: config.Temperature,
		MaxTokens:   config.MaxTokens,
		TopP:        config.TopP,
		Seed:        config.Seed,
		Stream:      false,
		Tools:       config.Tools,
	}
}
