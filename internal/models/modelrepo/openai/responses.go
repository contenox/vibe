package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
)

// request payloads and response parsing for the OpenAI Responses API.

type openAIResponsesRequest struct {
	Model           string                    `json:"model"`
	Input           []openAIResponseInput     `json:"input"`
	Instructions    string                    `json:"instructions,omitempty"`
	MaxOutputTokens *int                      `json:"max_output_tokens,omitempty"`
	Temperature     *float64                  `json:"temperature,omitempty"`
	TopP            *float64                  `json:"top_p,omitempty"`
	Seed            *int                      `json:"seed,omitempty"`
	Reasoning       *openAIResponsesReasoning `json:"reasoning,omitempty"`
	Tools           []openAIResponsesTool     `json:"tools,omitempty"`
	ToolChoice      string                    `json:"tool_choice,omitempty"`
	Stream          bool                      `json:"stream,omitempty"`
	// Store is always sent as false: OpenAI must not retain responses
	// server-side for this runtime. Pointer so the explicit false serializes.
	Store *bool `json:"store,omitempty"`
	// PromptCacheKey routes requests with the same key to the same cache
	// shard (cache metadata only, never model-visible). Same semantics as the
	// chat-completions field.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

type openAIResponsesReasoning struct {
	Effort string `json:"effort,omitempty"`
	// Summary requests reasoning summaries in the response output ("auto");
	// without it the API returns no reasoning content at all.
	Summary string `json:"summary,omitempty"`
}

type openAIResponsesTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
	Strict      bool   `json:"strict"`
}

type openAIResponseInput struct {
	Type string `json:"type"`
	// message fields
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`
	// function_call fields (assistant tool-call history)
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output fields (tool result)
	Output string `json:"output,omitempty"`
}

type openAIResponse struct {
	// Output carries the response items, including "reasoning" items whose
	// summary parts are the actual reasoning content. The wire also has a
	// top-level `reasoning` field, but it is a request-config echo and must
	// never be read as reasoning output.
	Output []openAIResponseOutputItem `json:"output"`
	// Usage is reported on the completed response object (and therefore on
	// the response.completed stream event). input_tokens already INCLUDES
	// cached tokens; the cached count is broken out under
	// input_tokens_details.cached_tokens (and gpt-5.6+ additionally reports
	// billed cache writes as cache_write_tokens).
	Usage *openAIResponsesUsage `json:"usage"`
	// Error is set on response.failed / response.incomplete stream events.
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	// Status is "completed" on a full response and "incomplete" on a
	// truncated one; IncompleteDetails.Reason then names why
	// ("max_output_tokens", "content_filter").
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// openAIResponsesUsage is the Responses API usage report. input_tokens is the
// TOTAL prompt count (cached included) — no normalization needed beyond
// copying the cached breakdown.
type openAIResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// CacheWriteTokens is reported by gpt-5.6+ models where cache writes are
	// billed; zero elsewhere.
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// neutralUsage maps the wire usage onto the neutral TokenUsage.
func (u *openAIResponsesUsage) neutralUsage() *modelrepo.TokenUsage {
	if u == nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return &modelrepo.TokenUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      total,
		CacheReadTokens:  u.InputTokensDetails.CachedTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

type openAIResponseOutputItem struct {
	Type      string                  `json:"type"`
	ID        string                  `json:"id"`
	Role      string                  `json:"role"`
	CallID    string                  `json:"call_id"`
	Name      string                  `json:"name"`
	Arguments string                  `json:"arguments"`
	Content   []openAIResponseContent `json:"content"`
	// Summary holds the reasoning summaries of an output item of type
	// "reasoning" (parts of type "summary_text"). This is where actual
	// reasoning surfaces; the response's top-level `reasoning` field is only a
	// config echo.
	Summary []openAIResponseContent `json:"summary"`
	Status  string                  `json:"status"`
	Phase   string                  `json:"phase"`
}

type openAIResponseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// openAIResponseInputContent is one element of a Responses input message's
// content-parts array, used only when the message carries image attachments.
// The Responses API names the parts input_text / input_image, and image_url is
// a bare data-URI string (not the nested {url} object the Chat Completions API
// uses).
type openAIResponseInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// openAIResponsesImageContent renders a message's text plus its image
// attachments as the Responses content-parts array: a leading input_text part
// (when present) then one input_image part per image, in attachment order.
func openAIResponsesImageContent(msg modelrepo.Message) []openAIResponseInputContent {
	parts := make([]openAIResponseInputContent, 0, len(msg.Images)+1)
	if msg.Content != "" {
		parts = append(parts, openAIResponseInputContent{Type: "input_text", Text: msg.Content})
	}
	for _, img := range msg.Images {
		parts = append(parts, openAIResponseInputContent{
			Type:     "input_image",
			ImageURL: imageDataURI(img.MimeType, img.Data),
		})
	}
	return parts
}

func buildOpenAIResponsesRequestWithCapabilities(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument, supportsThink bool) (openAIResponsesRequest, map[string]string) {
	req := openAIResponsesRequest{
		Model: modelName,
	}
	// Never let OpenAI retain responses server-side.
	storeFalse := false
	req.Store = &storeFalse

	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}

	req.Temperature = cfg.Temperature
	req.MaxOutputTokens = cfg.MaxTokens
	req.TopP = cfg.TopP
	req.Seed = cfg.Seed

	// Session cache-shard routing; cache metadata only (see the field doc).
	if cfg.CacheHints != nil && cfg.CacheHints.SessionKey != "" {
		req.PromptCacheKey = cfg.CacheHints.SessionKey
	}

	if supportsThink {
		reasoningEffort := openAIReasoningEffort(modelName, cfg.Think)
		if reasoningEffort != "" && reasoningEffort != "none" {
			req.Reasoning = &openAIResponsesReasoning{
				Effort: reasoningEffort,
				// Request summaries explicitly; without this the reasoning of
				// GPT-5-class models never surfaces in the output items.
				Summary: "auto",
			}
		} else if reasoningEffort == "none" {
			req.Reasoning = &openAIResponsesReasoning{Effort: reasoningEffort}
		}
	}

	if openAIShouldOmitSamplingParams(modelName, func() string {
		if req.Reasoning == nil {
			return ""
		}
		return req.Reasoning.Effort
	}()) {
		req.Temperature = nil
		req.TopP = nil
	}

	nameMap := make(map[string]string)
	origToSanitized := map[string]string{}
	if len(cfg.Tools) > 0 {
		seen := map[string]int{}
		tools := make([]openAIResponsesTool, 0, len(cfg.Tools))
		for i, t := range cfg.Tools {
			if strings.ToLower(t.Type) != "function" || t.Function == nil {
				continue
			}
			orig := t.Function.Name
			name := sanitizeToolName(orig)
			if name == "" {
				name = fmt.Sprintf("tool_%d", i)
			}
			name = uniquifyToolName(seen, name)
			nameMap[name] = orig
			origToSanitized[orig] = name
			tools = append(tools, openAIResponsesTool{
				Type:        "function",
				Name:        name,
				Description: t.Function.Description,
				Parameters:  openAIResponsesToolParameters(t.Function.Parameters),
				Strict:      false,
			})
		}
		if len(tools) > 0 {
			req.Tools = tools
		}
	}

	// Hoist system messages into the top-level instructions field.
	var systemParts []string
	for _, msg := range messages {
		if strings.TrimSpace(msg.Role) == "system" && msg.Content != "" {
			systemParts = append(systemParts, msg.Content)
		}
	}
	if len(systemParts) > 0 {
		req.Instructions = strings.Join(systemParts, "\n\n")
	}

	input := make([]openAIResponseInput, 0, len(messages))
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)

		switch role {
		case "system":
			// Already hoisted to Instructions above.
			continue

		case "tool":
			// Tool result → function_call_output item, correlated by call_id.
			input = append(input, openAIResponseInput{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: msg.Content,
			})
			continue

		case "assistant", "model":
			// If there is text, emit it as a regular assistant message first.
			if msg.Content != "" {
				input = append(input, openAIResponseInput{
					Type:    "message",
					Role:    "assistant",
					Content: msg.Content,
				})
			}
			// Each tool call becomes a function_call item so the model can
			// correlate it with the following function_call_output items.
			for _, tc := range msg.ToolCalls {
				name := tc.Function.Name
				if san, ok := origToSanitized[name]; ok && san != "" {
					name = san
				} else {
					name = sanitizeToolName(name)
					if name == "" {
						name = "tool"
					}
				}
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				input = append(input, openAIResponseInput{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      name,
					Arguments: args,
				})
			}
			continue

		case "user", "developer":
		default:
			role = "user"
		}

		if msg.Content == "" && len(msg.Images) == 0 {
			continue
		}
		var content any = msg.Content
		if len(msg.Images) > 0 {
			content = openAIResponsesImageContent(msg)
		}
		input = append(input, openAIResponseInput{
			Type:    "message",
			Role:    role,
			Content: content,
		})
	}
	req.Input = input

	return req, nameMap
}

// responsesReasoningSummaryText collects the reasoning summaries from output
// items of type "reasoning" (summary parts of type "summary_text").
func responsesReasoningSummaryText(resp *openAIResponse) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	for _, item := range resp.Output {
		if strings.ToLower(item.Type) != "reasoning" {
			continue
		}
		for _, part := range item.Summary {
			if part.Type == "summary_text" && part.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

func openAIResponsesToolParameters(params any) any {
	if params == nil {
		return map[string]any{}
	}
	return params
}

func parseOpenAIResponsesResponse(nameMap map[string]string, raw []byte) (modelrepo.ChatResult, error) {
	var resp openAIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return modelrepo.ChatResult{}, fmt.Errorf("responses: decode response: %w", err)
	}
	return parseOpenAIResponsesResponseFromObject(nameMap, resp)
}

func parseOpenAIResponsesResponseFromObject(nameMap map[string]string, response openAIResponse) (modelrepo.ChatResult, error) {
	resp := response

	if len(resp.Output) == 0 {
		return modelrepo.ChatResult{}, fmt.Errorf("responses: empty output")
	}

	var textBuilder strings.Builder
	var thinkingBuilder strings.Builder
	var toolCalls []modelrepo.ToolCall
	role := "assistant"

	for _, item := range resp.Output {
		switch strings.ToLower(item.Type) {
		case "reasoning":
			// Reasoning summaries live on output items of type "reasoning" as
			// summary parts of type "summary_text" — NOT on the response's
			// top-level `reasoning` field, which merely echoes the request
			// config.
			for _, part := range item.Summary {
				if part.Type == "summary_text" && part.Text != "" {
					if thinkingBuilder.Len() > 0 {
						thinkingBuilder.WriteString("\n\n")
					}
					thinkingBuilder.WriteString(part.Text)
				}
			}
		case "message":
			if item.Role != "" {
				role = item.Role
			}
			for _, chunk := range item.Content {
				if chunk.Type == "output_text" && chunk.Text != "" {
					textBuilder.WriteString(chunk.Text)
				}
			}
		case "function_call":
			tcID := item.CallID
			if tcID == "" {
				tcID = item.ID
			}
			name := item.Name
			if orig, ok := nameMap[name]; ok && orig != "" {
				name = orig
			}
			args := strings.TrimSpace(item.Arguments)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, modelrepo.ToolCall{
				ID:   tcID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      name,
					Arguments: args,
				},
			})
		}
	}

	if textBuilder.Len() == 0 && len(toolCalls) == 0 {
		return modelrepo.ChatResult{}, fmt.Errorf("responses: empty output")
	}

	// A truncated response reports status "incomplete" with the reason in
	// incomplete_details — surfaced verbatim as the finish reason so the
	// engine can tell a truncated success from a complete one.
	finishReason := ""
	if resp.Status == "incomplete" && resp.IncompleteDetails != nil {
		finishReason = resp.IncompleteDetails.Reason
	}

	return modelrepo.ChatResult{
		Message: modelrepo.Message{
			Role:     role,
			Content:  textBuilder.String(),
			Thinking: strings.TrimSpace(thinkingBuilder.String()),
		},
		ToolCalls:    toolCalls,
		Usage:        resp.Usage.neutralUsage(),
		FinishReason: finishReason,
	}, nil
}
