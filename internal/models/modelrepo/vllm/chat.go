package vllm

import (
	"context"
	"fmt"
	"net/http"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type VLLMChatClient struct {
	vLLMClient
}

func NewVLLMChatClient(ctx context.Context, baseURL, modelName string, contextLength, maxOutputTokens int, httpClient *http.Client, apiKey string, canThink bool, tracker libtracker.ActivityTracker) (modelrepo.LLMChatClient, error) {
	client := &VLLMChatClient{
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

	client.maxTokens = min(contextLength, 2048)
	return client, nil
}

func (c *VLLMChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "vllm", "model", c.modelName)
	defer end()

	request, nameMap := buildChatRequest(c.modelName, messages, args, c.canThink)
	c.clampChatRequest(&request)

	var response chatResponse

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
		Role:     choice.Message.Role,
		Content:  choice.Message.Content,
		Thinking: choice.Message.Thinking(),
	}

	// Convert tool calls
	toolCalls := convertChatToolCalls(choice.Message.ToolCalls, nameMap)

	result := modelrepo.ChatResult{
		Message:   message,
		ToolCalls: toolCalls,
		// vLLM reports no usable cache dimension per request (the V1 engine's
		// cached_tokens detail is broken/null — vllm#44961), so the cache
		// fields stay zero; Automatic Prefix Caching hits are measured
		// server-side (vllm:prefix_cache_* counters) and show up as TTFT.
		Usage: &modelrepo.TokenUsage{
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens:      response.Usage.TotalTokens,
		},
	}

	result.FinishReason = choice.FinishReason
	switch choice.FinishReason {
	case "stop", "tool_calls", "length":
		// "length" is a truncated SUCCESS, same contract as the streaming
		// assembler: the partial content is real, and the verbatim finish
		// reason lets the engine surface the truncation instead of the old
		// behavior of discarding the content behind an opaque error.
		reportChange("chat_completed", map[string]any{
			"finish_reason":    choice.FinishReason,
			"content_length":   len(message.Content),
			"thinking_length":  len(message.Thinking),
			"tool_calls_count": len(toolCalls),
		})
		return result, nil
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
