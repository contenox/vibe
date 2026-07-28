package vllm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

func NewVLLMPromptClient(ctx context.Context, baseURL, modelName string, contextLength, maxOutputTokens int, httpClient *http.Client, apiKey string, canThink bool, tracker libtracker.ActivityTracker) (modelrepo.LLMPromptExecClient, error) {
	if httpClient == nil {
		httpClient = modelrepo.SharedHTTPClient
	}

	client := &vLLMPromptClient{
		vLLMClient: vLLMClient{
			baseURL:         baseURL,
			httpClient:      httpClient,
			modelName:       modelName,
			maxOutputTokens: maxOutputTokens,
			canThink:        canThink,
			apiKey:          apiKey,
			tracker:         tracker,
		},
	}
	client.maxTokens = 2048
	if contextLength > 0 {
		client.maxTokens = min(contextLength, client.maxTokens)
	}
	client.maxTokens, _ = modelrepo.ClampMaxOutputTokens(client.maxTokens, client.maxOutputTokens)

	return client, nil
}

func (c *vLLMClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *modelrepo.TokenUsage, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "prompt", "vllm", "model", c.modelName)
	defer end()

	messages := []modelrepo.Message{
		{Role: "system", Content: systemInstruction},
		{Role: "user", Content: prompt},
	}

	request, _ := buildChatRequest(c.modelName, messages, []modelrepo.ChatArgument{
		modelrepo.WithTemperature(float64(temperature)),
		modelrepo.WithMaxTokens(c.maxTokens),
	})
	c.clampChatRequest(&request)

	var response chatResponse
	if err := c.sendRequest(ctx, "/v1/chat/completions", request, &response); err != nil {
		reportErr(err)
		return "", nil, err
	}

	if len(response.Choices) == 0 {
		err := fmt.Errorf("no completion choices returned from vLLM for model %s", c.modelName)
		reportErr(err)
		return "", nil, err
	}

	choice := response.Choices[0]
	switch choice.FinishReason {
	case "stop":
		if choice.Message.Content == "" {
			err := fmt.Errorf("empty content from model %s despite normal completion", c.modelName)
			reportErr(err)
			return "", nil, err
		}
		reportChange("prompt_completed", map[string]any{
			"finish_reason":     "stop",
			"content_length":    len(choice.Message.Content),
			"thinking_length":   len(choice.Message.Thinking()),
			"prompt_tokens":     response.Usage.PromptTokens,
			"completion_tokens": response.Usage.CompletionTokens,
		})
		// Same accounting as the chat path: vLLM reports no usable cache
		// dimension per request, so the cache fields stay zero.
		usage := &modelrepo.TokenUsage{
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens:      response.Usage.TotalTokens,
		}
		return choice.Message.Content, usage, nil
	case "length":
		err := fmt.Errorf("token limit reached for model %s (partial response: %q)", c.modelName, choice.Message.Content)
		reportErr(err)
		return "", nil, err
	case "content_filter":
		err := fmt.Errorf("content filtered for model %s (partial response: %q)", c.modelName, choice.Message.Content)
		reportErr(err)
		return "", nil, err
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", choice.FinishReason, c.modelName)
		reportErr(err)
		return "", nil, err
	}
}
