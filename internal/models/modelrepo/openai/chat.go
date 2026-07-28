package openai

import (
	"context"
	"fmt"

	"github.com/contenox/contenox/internal/models/modelrepo"
)

type OpenAIChatClient struct {
	openAIClient
}

// openAIChatCompletionResponse matches the /v1/chat/completions JSON body.
// reasoning_content is a best-effort field exposed by some OpenAI-compatible
// backends; official OpenAI reasoning summaries live in the Responses API instead.
type openAIChatCompletionResponse struct {
	Choices []openAIChatCompletionChoice `json:"choices"`
	Usage   *openAIChatCompletionUsage   `json:"usage"`
}

// openAIChatCompletionUsage is the chat-completions usage report. prompt_tokens
// already includes cached tokens; the cached count is broken out separately
// under prompt_tokens_details.cached_tokens.
type openAIChatCompletionUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *openAIChatCompletionUsage) neutralUsage() *modelrepo.TokenUsage {
	if u == nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return &modelrepo.TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      total,
		CacheReadTokens:  u.PromptTokensDetails.CachedTokens,
	}
}

type openAIChatCompletionChoice struct {
	Index        int                     `json:"index"`
	Message      openAIChatCompletionMsg `json:"message"`
	FinishReason string                  `json:"finish_reason"`
}

type openAIChatCompletionMsg struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (c *OpenAIChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "openai", "model", c.modelName)
	defer end()

	if openAIUsesResponsesEndpoint(c.modelName) {
		req, nameMap := buildOpenAIResponsesRequestWithCapabilities(c.modelName, messages, args, c.supportsThink)
		c.clampResponsesMaxOutputTokens(&req)
		var response openAIResponse
		if err := c.sendRequest(ctx, "/responses", req, &response); err != nil {
			reportErr(err)
			return modelrepo.ChatResult{}, err
		}
		result, err := parseOpenAIResponsesResponseFromObject(nameMap, response)
		if err != nil {
			reportErr(err)
			return modelrepo.ChatResult{}, err
		}
		reportChange("chat_completed", result)
		return result, nil
	}

	req, nameMap := buildOpenAIRequestWithCapabilities(c.modelName, messages, args, c.supportsThink)
	c.clampChatMaxOutputTokens(&req)
	var response openAIChatCompletionResponse

	if err := c.sendRequest(ctx, "/chat/completions", req, &response); err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	if len(response.Choices) == 0 {
		err := fmt.Errorf("no chat completion choices returned from OpenAI for model %s", c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	choice := response.Choices[0]
	if choice.Message.Content == "" && len(choice.Message.ToolCalls) == 0 && choice.Message.ReasoningContent == "" {
		err := fmt.Errorf("empty content from model %s despite normal completion. Finish reason: %s", c.modelName, choice.FinishReason)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	message := modelrepo.Message{
		Role:     choice.Message.Role,
		Content:  choice.Message.Content,
		Thinking: choice.Message.ReasoningContent,
	}

	// Translate sanitized tool names back to what the caller originally provided.
	var toolCalls []modelrepo.ToolCall
	for _, tc := range choice.Message.ToolCalls {
		name := tc.Function.Name
		if orig, ok := nameMap[name]; ok && orig != "" {
			name = orig
		}
		toolCalls = append(toolCalls, modelrepo.ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	result := modelrepo.ChatResult{
		Message:      message,
		ToolCalls:    toolCalls,
		Usage:        response.Usage.neutralUsage(),
		FinishReason: choice.FinishReason,
	}
	reportChange("chat_completed", result)
	return result, nil
}

var _ modelrepo.LLMChatClient = (*OpenAIChatClient)(nil)
