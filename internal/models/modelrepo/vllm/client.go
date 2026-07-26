package vllm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
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
	baseURL         string
	httpClient      *http.Client
	modelName       string
	maxTokens       int
	maxOutputTokens int
	canThink        bool
	apiKey          string
	tracker         libtracker.ActivityTracker
}

type chatRequest struct {
	Model string `json:"model"`
	// Messages is []any so text/tool turns serialize as the neutral
	// modelrepo.Message (unchanged wire shape) while image-bearing turns
	// serialize as the OpenAI content-parts form (see toVLLMRequestMessages).
	Messages           []any            `json:"messages"`
	Temperature        *float64         `json:"temperature,omitempty"`
	MaxTokens          *int             `json:"max_tokens,omitempty"`
	TopP               *float64         `json:"top_p,omitempty"`
	Seed               *int             `json:"seed,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	Tools              []modelrepo.Tool `json:"tools,omitempty"`
	ReasoningEffort    string           `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any   `json:"chat_template_kwargs,omitempty"`
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

// vllmImageMessage is the OpenAI content-parts form of a message sent to
// vLLM's chat/completions endpoint when it carries image attachments.
type vllmImageMessage struct {
	Role    string            `json:"role"`
	Content []vllmContentPart `json:"content"`
}

type vllmContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *vllmImageURL `json:"image_url,omitempty"`
}

type vllmImageURL struct {
	URL string `json:"url"`
}

// toVLLMRequestMessages maps neutral messages to the request wire form: an
// image-bearing message becomes the OpenAI content-parts shape, while every
// other message stays a raw modelrepo.Message so text and tool behavior keep
// their existing wire shape byte-for-byte.
func toVLLMRequestMessages(messages []modelrepo.Message) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		if len(m.Images) == 0 {
			out = append(out, m)
			continue
		}
		parts := make([]vllmContentPart, 0, len(m.Images)+1)
		if m.Content != "" {
			parts = append(parts, vllmContentPart{Type: "text", Text: m.Content})
		}
		for _, img := range m.Images {
			parts = append(parts, vllmContentPart{
				Type:     "image_url",
				ImageURL: &vllmImageURL{URL: vllmImageDataURI(img.MimeType, img.Data)},
			})
		}
		out = append(out, vllmImageMessage{Role: m.Role, Content: parts})
	}
	return out
}

func vllmImageDataURI(mimeType string, data []byte) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func buildChatRequest(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument, canThink ...bool) chatRequest {
	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	return buildChatRequestFromConfig(modelName, messages, config, canThink...)
}

func buildChatRequestFromConfig(modelName string, messages []modelrepo.Message, config *modelrepo.ChatConfig, canThink ...bool) chatRequest {
	req := chatRequest{
		Model:       modelName,
		Messages:    toVLLMRequestMessages(messages),
		Temperature: config.Temperature,
		MaxTokens:   config.MaxTokens,
		TopP:        config.TopP,
		Seed:        config.Seed,
		Stream:      false,
		Tools:       config.Tools,
	}

	if vllmThinkingAllowed(canThink) {
		if effort, ok := vllmReasoningEffort(config.Think); ok {
			req.ReasoningEffort = effort
			req.ChatTemplateKwargs = map[string]any{"enable_thinking": effort != "none"}
		}
	}

	return req
}

func (c *vLLMClient) clampChatRequest(req *chatRequest) {
	if req == nil {
		return
	}
	req.MaxTokens = modelrepo.ClampMaxOutputTokensPtr(req.MaxTokens, c.maxOutputTokens)
}

func vllmThinkingAllowed(canThink []bool) bool {
	return len(canThink) == 0 || canThink[0]
}

func vllmReasoningEffort(think *string) (string, bool) {
	level, ok, err := reasoning.NormalizeOptional(valueOfStringPtr(think))
	if err != nil || !ok || level == reasoning.Auto {
		return "", false
	}
	switch level {
	case reasoning.Off:
		return "none", true
	case reasoning.Minimal, reasoning.Low:
		return reasoning.Low, true
	case reasoning.Medium:
		return reasoning.Medium, true
	case reasoning.High, reasoning.XHigh:
		return reasoning.High, true
	default:
		return "", false
	}
}

func valueOfStringPtr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
