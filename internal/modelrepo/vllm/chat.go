package vllm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

type VLLMChatClient struct {
	vLLMClient
}

func NewVLLMChatClient(ctx context.Context, baseURL, modelName string, contextLength int, httpClient *http.Client, apiKey string, tracker libtracker.ActivityTracker) (modelrepo.LLMChatClient, error) {
	client := &VLLMChatClient{
		vLLMClient: vLLMClient{
			baseURL:    baseURL,
			httpClient: httpClient,
			modelName:  modelName,
			apiKey:     apiKey,
			tracker:    tracker,
		},
	}

	client.maxTokens = min(contextLength, 2048)
	return client, nil
}

func (c *VLLMChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "vllm", "model", c.modelName)
	defer end()

	request := buildChatRequest(c.modelName, messages, args)

	var response struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}

	if err := c.sendRequest(ctx, "/v1/chat/completions", request, &response); err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	if len(response.Choices) == 0 {
		err := fmt.Errorf("no completion choices returned")
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	choice := response.Choices[0]

	// Convert to our format
	message := modelrepo.Message{
		Role:    choice.Message.Role,
		Content: choice.Message.Content,
	}

	// Convert tool calls
	var toolCalls []modelrepo.ToolCall
	for _, tc := range choice.Message.ToolCalls {
		toolCalls = append(toolCalls, modelrepo.ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	result := modelrepo.ChatResult{
		Message:   message,
		ToolCalls: toolCalls,
	}

	switch choice.FinishReason {
	case "stop":
		reportChange("chat_completed", map[string]any{
			"finish_reason":    "stop",
			"content_length":   len(message.Content),
			"tool_calls_count": len(toolCalls),
		})
		return result, nil
	case "length":
		err := fmt.Errorf("token limit reached")
		reportErr(err)
		return modelrepo.ChatResult{}, err
	case "content_filter":
		err := fmt.Errorf("content filtered")
		reportErr(err)
		return modelrepo.ChatResult{}, err
	default:
		err := fmt.Errorf("unexpected completion reason: %s", choice.FinishReason)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
}

var _ modelrepo.LLMChatClient = (*VLLMChatClient)(nil)
