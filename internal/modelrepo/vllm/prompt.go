package vllm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

// NewVLLMPromptClient creates a new prompt client
func NewVLLMPromptClient(ctx context.Context, baseURL, modelName string, contextLength int, httpClient *http.Client, apiKey string, tracker libtracker.ActivityTracker) (modelrepo.LLMPromptExecClient, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	client := &vLLMPromptClient{
		vLLMClient: vLLMClient{
			baseURL:    baseURL,
			httpClient: httpClient,
			modelName:  modelName,
			apiKey:     apiKey,
			tracker:    tracker,
		},
	}
	client.maxTokens = 2048
	if contextLength > 0 {
		client.maxTokens = min(contextLength, client.maxTokens)
	}

	return client, nil
}

// Prompt implements LLMPromptExecClient interface
func (c *vLLMClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "prompt", "vllm", "model", c.modelName)
	defer end()

	messages := []modelrepo.Message{
		{Role: "system", Content: systemInstruction},
		{Role: "user", Content: prompt},
	}

	// Convert to pointers for API request
	tempVal := float64(temperature)
	maxTokensVal := c.maxTokens

	request := chatRequest{
		Model:       c.modelName,
		Messages:    messages,
		Temperature: &tempVal,
		MaxTokens:   &maxTokensVal,
		Stream:      false,
	}

	// Send request to the chat completions endpoint
	var response chatResponse
	if err := c.sendRequest(ctx, "/v1/chat/completions", request, &response); err != nil {
		reportErr(err)
		return "", err
	}

	// Handle response
	if len(response.Choices) == 0 {
		err := fmt.Errorf("no completion choices returned from vLLM for model %s", c.modelName)
		reportErr(err)
		return "", err
	}

	choice := response.Choices[0]
	switch choice.FinishReason {
	case "stop":
		if choice.Message.Content == "" {
			err := fmt.Errorf("empty content from model %s despite normal completion", c.modelName)
			reportErr(err)
			return "", err
		}
		reportChange("prompt_completed", map[string]any{
			"finish_reason":     "stop",
			"content_length":    len(choice.Message.Content),
			"prompt_tokens":     response.Usage.PromptTokens,
			"completion_tokens": response.Usage.CompletionTokens,
		})
		return choice.Message.Content, nil
	case "length":
		err := fmt.Errorf("token limit reached for model %s (partial response: %q)", c.modelName, choice.Message.Content)
		reportErr(err)
		return "", err
	case "content_filter":
		err := fmt.Errorf("content filtered for model %s (partial response: %q)", c.modelName, choice.Message.Content)
		reportErr(err)
		return "", err
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", choice.FinishReason, c.modelName)
		reportErr(err)
		return "", err
	}
}

type chatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Choices []struct {
		Index        int               `json:"index"`
		Message      modelrepo.Message `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}
