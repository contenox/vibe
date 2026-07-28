package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/beam/internal/kernel/reasoning"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type openAIClient struct {
	baseURL         string
	apiKey          string
	httpClient      *http.Client
	modelName       string
	maxTokens       int
	maxOutputTokens int
	tracker         libtracker.ActivityTracker
	supportsThink   bool
}

type openAIChatRequest struct {
	Model               string           `json:"model"`
	Messages            []apiChatMessage `json:"messages"`
	Temperature         *float64         `json:"temperature,omitempty"`
	MaxCompletionTokens *int             `json:"max_completion_tokens,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	Seed                *int             `json:"seed,omitempty"`
	Stream              bool             `json:"stream,omitempty"`
	// StreamOptions requests the trailing usage chunk; only set when Stream is true.
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
	Tools         []openAITool         `json:"tools,omitempty"`
	// ReasoningEffort maps modelrepo.WithThink onto OpenAI's chat-completions
	// `reasoning_effort` parameter; supported values are model-dependent.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// PromptCacheKey routes requests with the same key to the same cache shard
	// (OpenAI's documented alternative to the deprecated `user` field). Cache
	// metadata only, never model-visible.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// apiChatMessage is the wire-format message sent to the OpenAI REST API.
// Content is `any` so it can carry a plain string (with null for tool-only
// assistant messages), or the content-parts array when the message has image
// attachments.
type apiChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []apiToolCallReq `json:"tool_calls,omitempty"`
}

// apiContentPart is one element of the chat/completions content-parts array,
// used only when a message carries image attachments.
type apiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *apiImageURL `json:"image_url,omitempty"`
}

type apiImageURL struct {
	URL string `json:"url"`
}

type apiToolCallReq struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function openAIFunction2 `json:"function"`
}

type openAIFunction2 struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAITool struct {
	Type     string         `json:"type"` // must be "function"
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string `json:"name"` // ^[a-zA-Z0-9_-]+$
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"` // JSON Schema
}

func (c *openAIClient) sendRequest(ctx context.Context, endpoint string, request any, response any) error {
	url := c.baseURL + endpoint

	tracker := c.tracker
	// Never log API key material in activity telemetry; trace logs are not secret-safe.
	auth := "none"
	if c.apiKey != "" {
		auth = "bearer_set"
	}
	reportErr, reportChange, end := tracker.Start(
		ctx,
		"http_request",
		"openai",
		"model", c.modelName,
		"endpoint", endpoint,
		"base_url", c.baseURL,
		"auth", auth,
	)
	defer end()

	var body []byte
	if request != nil {
		var err error
		body, err = json.Marshal(request)
		if err != nil {
			err = fmt.Errorf("failed to marshal request: %w", err)
			reportErr(err)
			return err
		}
	}

	// Bounded end-to-end; retried on 429/529/5xx with Retry-After honored.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()

	resp, err := modelrepo.DoWithRetry(ctx, c.httpClient, func() (*http.Request, error) {
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", url, reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		return req, nil
	})
	if err != nil {
		err = fmt.Errorf("HTTP request failed for model %s: %w", c.modelName, err)
		reportErr(err)
		return err
	}
	defer resp.Body.Close()

	reportChange("http_response", map[string]any{
		"status_code": resp.StatusCode,
		"headers":     resp.Header,
	})

	if resp.StatusCode != http.StatusOK {
		var errorResponse struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		code, message := "", ""
		if jsonErr := json.Unmarshal(bodyBytes, &errorResponse); jsonErr == nil && errorResponse.Error.Message != "" {
			code = fmt.Sprintf("%v", errorResponse.Error.Code)
			message = errorResponse.Error.Message
			err = fmt.Errorf("OpenAI API returned non-200 status: %d, Type: %s, Code: %v, Message: %s for model %s",
				resp.StatusCode, errorResponse.Error.Type, errorResponse.Error.Code, errorResponse.Error.Message, c.modelName)
		} else {
			message = string(bodyBytes)
			err = fmt.Errorf("OpenAI API returned non-200 status: %d, body: %s for model %s",
				resp.StatusCode, string(bodyBytes), c.modelName)
		}
		err = modelrepo.ClassifyProviderError(err, resp.StatusCode, code, message)
		reportErr(err)
		return err
	}

	if response != nil {
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			err = fmt.Errorf("failed to decode response for model %s: %w", c.modelName, err)
			reportErr(err)
			return err
		}
	}

	reportChange("request_completed", nil)
	return nil
}

// buildOpenAIRequest builds a compliant request and sanitizes tool names to
// OpenAI's pattern (^[a-zA-Z0-9_-]+$), returning a sanitized->original map so
// callers can translate names back. It also sanitizes tool_calls[].function.name
// in message history, since taskengine's "toolsName.toolName" qualified names
// contain a dot that violates OpenAI's pattern.
func buildOpenAIRequest(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument) (openAIChatRequest, map[string]string) {
	return buildOpenAIRequestWithCapabilities(modelName, messages, args, true)
}

func (c *openAIClient) clampChatMaxOutputTokens(req *openAIChatRequest) {
	if req == nil {
		return
	}
	req.MaxCompletionTokens = modelrepo.ClampMaxOutputTokensPtr(req.MaxCompletionTokens, c.maxOutputTokens)
}

func (c *openAIClient) clampResponsesMaxOutputTokens(req *openAIResponsesRequest) {
	if req == nil {
		return
	}
	req.MaxOutputTokens = modelrepo.ClampMaxOutputTokensPtr(req.MaxOutputTokens, c.maxOutputTokens)
}

func buildOpenAIRequestWithCapabilities(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument, supportsThink bool) (openAIChatRequest, map[string]string) {
	req := openAIChatRequest{
		Model: modelName,
	}

	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}
	req.Temperature = cfg.Temperature
	req.MaxCompletionTokens = cfg.MaxTokens
	req.TopP = cfg.TopP
	req.Seed = cfg.Seed

	if supportsThink {
		req.ReasoningEffort = openAIReasoningEffort(modelName, cfg.Think)
	}

	// OpenAI prefix caching is automatic (≥1024 tokens); the session cache
	// key only steers shard routing so one session's requests hit one cache.
	if cfg.CacheHints != nil && cfg.CacheHints.SessionKey != "" {
		req.PromptCacheKey = cfg.CacheHints.SessionKey
	}

	// Sampling parameter support depends on both model family and reasoning mode.
	if openAIShouldOmitSamplingParams(modelName, req.ReasoningEffort) {
		req.Temperature = nil
		req.TopP = nil
	}

	// Convert tools to OpenAI tools with sanitized/unique function names.
	nameMap := make(map[string]string) // sanitized -> original
	seen := map[string]int{}
	if len(cfg.Tools) > 0 {
		tools := make([]openAITool, 0, len(cfg.Tools))
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
			tools = append(tools, openAITool{
				Type: "function",
				Function: openAIFunction{
					Name:        name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
		if len(tools) > 0 {
			req.Tools = tools
		}
	}

	// Reverse map: original tool name -> sanitized name, for rewriting history.
	origToSanitized := make(map[string]string, len(nameMap))
	for san, orig := range nameMap {
		origToSanitized[orig] = san
	}

	apiMsgs := make([]apiChatMessage, 0, len(messages))
	for _, msg := range messages {
		apiMsg := apiChatMessage{
			Role:       msg.Role,
			ToolCallID: msg.ToolCallID,
		}
		switch {
		case len(msg.Images) > 0:
			// Image attachments force the content-parts array form.
			apiMsg.Content = openAIImageContent(msg)
		case msg.Content == "" && len(msg.ToolCalls) > 0:
			// Assistant messages that only carry tool calls send null content.
			apiMsg.Content = nil
		default:
			apiMsg.Content = msg.Content
		}

		if len(msg.ToolCalls) > 0 {
			apiMsg.ToolCalls = make([]apiToolCallReq, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				name := tc.Function.Name
				if san, ok := origToSanitized[name]; ok {
					name = san
				} else {
					name = sanitizeToolName(name)
				}
				apiMsg.ToolCalls = append(apiMsg.ToolCalls, apiToolCallReq{
					ID:   tc.ID,
					Type: tc.Type,
					Function: openAIFunction2{
						Name:      name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		apiMsgs = append(apiMsgs, apiMsg)
	}
	req.Messages = apiMsgs

	return req, nameMap
}

// openAIImageContent renders a message as the chat/completions content-parts
// array: a leading text part (when present), then one image_url part per
// image as an inline base64 data URI.
func openAIImageContent(msg modelrepo.Message) []apiContentPart {
	parts := make([]apiContentPart, 0, len(msg.Images)+1)
	if msg.Content != "" {
		parts = append(parts, apiContentPart{Type: "text", Text: msg.Content})
	}
	for _, img := range msg.Images {
		parts = append(parts, apiContentPart{
			Type:     "image_url",
			ImageURL: &apiImageURL{URL: imageDataURI(img.MimeType, img.Data)},
		})
	}
	return parts
}

// imageDataURI builds the data:<mime>;base64,<payload> URI OpenAI accepts for
// inline image bytes; shared by the chat/completions and Responses builders.
func imageDataURI(mimeType string, data []byte) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// openAIAPIBaseModelID returns the model id segment OpenAI expects, without provider/namespace
// prefixes (e.g. "openai/gpt-5" -> "gpt-5"). Runtime state may store namespaced ids.
func openAIAPIBaseModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

// openAIUsesResponsesEndpoint reports whether this model requires the OpenAI
// Responses API (POST /v1/responses) rather than /chat/completions. GPT-5
// family models are routed to /responses.
func openAIUsesResponsesEndpoint(model string) bool {
	base := openAIAPIBaseModelID(model)
	return strings.HasPrefix(base, "gpt-5")
}

func openAIReasoningEffort(model string, think *string) string {
	if think == nil {
		return ""
	}

	level, ok, err := reasoning.NormalizeOptional(*think)
	if err != nil || !ok || level == reasoning.Auto {
		return ""
	}
	if level == reasoning.Off {
		if openAIModelSupportsNoneReasoning(model) {
			return "none"
		}
		return ""
	}

	switch level {
	case reasoning.Minimal:
		if openAIModelSupportsMinimalReasoning(model) {
			return "minimal"
		}
		return "low"
	case reasoning.Low, reasoning.Medium:
		if openAIModelOnlyHighReasoning(model) {
			return "high"
		}
		return level
	case reasoning.High:
		return "high"
	case reasoning.XHigh:
		if openAIModelSupportsXHighReasoning(model) {
			return "xhigh"
		}
		return "high"
	default:
		return ""
	}
}

func openAIShouldOmitSamplingParams(model, reasoningEffort string) bool {
	base := openAIAPIBaseModelID(model)
	switch {
	case strings.HasPrefix(base, "o"):
		return reasoningEffort != ""
	case strings.HasPrefix(base, "gpt-5"):
		return !openAIGPT5AllowsSamplingParams(model, reasoningEffort)
	default:
		return false
	}
}

func openAIGPT5AllowsSamplingParams(model, reasoningEffort string) bool {
	if !strings.HasPrefix(openAIAPIBaseModelID(model), "gpt-5") {
		return true
	}
	return openAIModelSupportsNoneReasoning(model) && (reasoningEffort == "" || reasoningEffort == "none")
}

func openAIModelOnlyHighReasoning(model string) bool {
	base := openAIAPIBaseModelID(model)
	return base == "gpt-5-pro" || strings.HasPrefix(base, "gpt-5-pro-")
}

func openAIModelSupportsNoneReasoning(model string) bool {
	base := openAIAPIBaseModelID(model)
	if openAIModelOnlyHighReasoning(base) {
		return false
	}
	return strings.HasPrefix(base, "gpt-5.1") ||
		strings.HasPrefix(base, "gpt-5.2") ||
		strings.HasPrefix(base, "gpt-5.3") ||
		strings.HasPrefix(base, "gpt-5.4")
}

func openAIModelSupportsMinimalReasoning(model string) bool {
	base := openAIAPIBaseModelID(model)
	if strings.HasPrefix(base, "gpt-5") {
		return openAIModelSupportsNoneReasoning(model) && !strings.HasPrefix(base, "gpt-5.1")
	}
	return false
}

func openAIModelSupportsXHighReasoning(model string) bool {
	base := openAIAPIBaseModelID(model)
	if openAIModelOnlyHighReasoning(model) {
		return false
	}
	return strings.HasPrefix(base, "gpt-5.2") ||
		strings.HasPrefix(base, "gpt-5.3") ||
		strings.HasPrefix(base, "gpt-5.4")
}

// sanitizeToolName replaces invalid characters with '_' and trims leading/trailing separators.
// Allowed: letters, digits, underscore, hyphen.
func sanitizeToolName(in string) string {
	if in == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	s = strings.Trim(s, "_-")
	return s
}

// uniquifyToolName ensures we don't send duplicate names (OpenAI recommends unique names).
func uniquifyToolName(seen map[string]int, name string) string {
	if _, ok := seen[name]; !ok {
		seen[name] = 1
		return name
	}
	i := seen[name]
	for {
		candidate := fmt.Sprintf("%s_%d", name, i)
		if _, ok := seen[candidate]; !ok {
			seen[name] = i + 1
			seen[candidate] = 1
			return candidate
		}
		i++
	}
}
