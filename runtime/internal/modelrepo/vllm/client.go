package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
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
	// ExtraBody passes provider-specific parameters (e.g. enable_thinking for Qwen3/Granite).
	// We intentionally defer vLLM-only request fields such as tool_choice,
	// parallel_tool_calls, response_format, structured_outputs, and /v1/responses
	// until modelrepo grows matching shared request fields.
	ExtraBody map[string]any `json:"extra_body,omitempty"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int          `json:"created"`
	Choices []chatChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Reasoning        string         `json:"reasoning,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

func (m chatMessage) Thinking() string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.ReasoningContent
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func convertChatToolCalls(toolCalls []chatToolCall) []modelrepo.ToolCall {
	out := make([]modelrepo.ToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		out = append(out, modelrepo.ToolCall{
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
	return out
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

	return buildChatRequestFromConfig(modelName, messages, config)
}

func buildChatRequestFromConfig(modelName string, messages []modelrepo.Message, config *modelrepo.ChatConfig) chatRequest {
	req := chatRequest{
		Model:       modelName,
		Messages:    messages,
		Temperature: config.Temperature,
		MaxTokens:   config.MaxTokens,
		TopP:        config.TopP,
		Seed:        config.Seed,
		Stream:      false,
		Tools:       config.Tools,
	}

	// Wire enable_thinking for Qwen3, Granite, and DeepSeek-V3.1 served via vLLM.
	// DeepSeek-R1 reasoning output is enabled server-side (--reasoning-parser deepseek_r1);
	// it doesn't need this flag but harmlessly ignores it.
	if thinkEnabled, ok := vllmThinkingEnabled(config.Think); ok {
		req.ExtraBody = map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": thinkEnabled},
		}
	}

	return req
}

func vllmThinkingEnabled(think *string) (bool, bool) {
	if think == nil {
		return false, false
	}

	switch *think {
	case "", "default":
		return false, false
	case "false", "none", "off", "disabled":
		return false, true
	default:
		return true, true
	}
}
