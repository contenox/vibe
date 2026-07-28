package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
)

type geminiClient struct {
	apiKey          string
	modelName       string
	baseURL         string
	httpClient      *http.Client
	maxTokens       int
	maxOutputTokens int
	canThink        bool
	tracker         libtracker.ActivityTracker
}

type geminiGenerateContentRequest struct {
	SystemInstruction *geminiSystemInstruction `json:"system_instruction,omitempty"`
	Contents          []geminiContent          `json:"contents"`
	GenerationConfig  *geminiGenerationConfig  `json:"generationConfig,omitempty"`
	Tools             []geminiToolRequest      `json:"tools,omitempty"`
}

type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	Seed            *int     `json:"seed,omitempty"`
	// ThinkingConfig controls extended thinking on Gemini 2.5+ models.
	// Use nil to omit (default behaviour, no thinking).
	ThinkingConfig *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// geminiThinkingConfig maps to Gemini thinking controls. Gemini 3 uses
// thinkingLevel; Gemini 2.5 uses thinkingBudget.
type geminiThinkingConfig struct {
	ThinkingBudget *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel  string `json:"thinkingLevel,omitempty"`
}

// sendRequest is the shared HTTP helper for Gemini clients.
func (c *geminiClient) sendRequest(ctx context.Context, endpoint string, request interface{}, response interface{}) error {
	fullURL := fmt.Sprintf("%s%s", c.baseURL, endpoint)

	tracker := c.tracker
	reportErr, reportChange, end := tracker.Start(
		ctx,
		"http_request",
		"gemini",
		"model", c.modelName,
		"endpoint", endpoint,
		"base_url", c.baseURL,
	)
	defer end()

	var body []byte
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			err = fmt.Errorf("failed to marshal request: %w", err)
			reportErr(err)
			return err
		}
		body = b
	}

	// Non-streaming call: bounded end-to-end, retried on 429/5xx with
	// Retry-After honored.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()
	resp, err := modelrepo.DoWithRetry(ctx, c.httpClient, func() (*http.Request, error) {
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Api-Key", c.apiKey)
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
		var eresp struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		respBody, _ := io.ReadAll(resp.Body)
		if jsonErr := json.Unmarshal(respBody, &eresp); jsonErr == nil && eresp.Error.Message != "" {
			// Gemini's documented error shapes: RESOURCE_EXHAUSTED → rate
			// limit, INVALID_ARGUMENT with token phrasing → context overflow.
			err = fmt.Errorf("gemini API error: %d %s - %s (model=%s url=%s)",
				resp.StatusCode, eresp.Error.Status, eresp.Error.Message, c.modelName, fullURL)
			err = modelrepo.ClassifyProviderError(err, resp.StatusCode, eresp.Error.Status, eresp.Error.Message)
			reportErr(err)
			return err
		}
		err = fmt.Errorf("gemini API error: %d - %s (model=%s url=%s)", resp.StatusCode, string(respBody), c.modelName, fullURL)
		err = modelrepo.ClassifyProviderError(err, resp.StatusCode, "", string(respBody))
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

// buildGeminiRequest builds a Gemini generateContent request from modelrepo
// messages and args. System messages are hoisted into systemInstruction here
// so every caller (chat and stream) sends them; a caller cannot forget and
// silently drop the system prompt.
func buildGeminiRequest(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument, canThink ...bool) (geminiGenerateContentRequest, error) {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}

	var systemInstruction *geminiSystemInstruction
	filtered := make([]modelrepo.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if m.Content != "" {
				systemInstruction = &geminiSystemInstruction{
					Parts: []geminiPart{{Text: m.Content}},
				}
			}
			continue
		}
		filtered = append(filtered, m)
	}
	messages = filtered

	var tools []geminiToolRequest
	if len(cfg.Tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(cfg.Tools))
		for _, t := range cfg.Tools {
			toolschema, err := geminiSanitiseSchema(t.Function.Parameters)
			if err != nil {
				return geminiGenerateContentRequest{}, err
			}
			if t.Type == "function" && t.Function != nil {
				decls = append(decls, geminiFunctionDeclaration{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					// Gemini rejects additionalProperties in function schemas.
					Parameters: toolschema,
				})
			}
		}
		if len(decls) > 0 {
			tools = append(tools, geminiToolRequest{
				FunctionDeclarations: decls,
			})
		}
	}

	contents := convertToGeminiMessages(messages)
	if len(contents) == 0 {
		return geminiGenerateContentRequest{}, fmt.Errorf("gemini: refusing to send empty contents for model %s after filtering %d message(s); provide at least one non-empty user/model/tool message, tool call, or tool response", modelName, len(messages))
	}

	req := geminiGenerateContentRequest{
		SystemInstruction: systemInstruction,
		Contents:          contents,
		GenerationConfig:  &geminiGenerationConfig{},
		Tools:             tools,
	}
	req.GenerationConfig.Temperature = cfg.Temperature
	req.GenerationConfig.TopP = cfg.TopP
	if cfg.MaxTokens != nil {
		req.GenerationConfig.MaxOutputTokens = cfg.MaxTokens
	}
	req.GenerationConfig.Seed = cfg.Seed

	if thinkingAllowed(canThink) {
		req.GenerationConfig.ThinkingConfig = geminiThinkingConfigForModel(modelName, cfg.Think)
	}

	return req, nil
}

func geminiThinkingConfigForModel(modelName string, think *string) *geminiThinkingConfig {
	level, ok, err := reasoning.NormalizeOptional(valueOfStringPtr(think))
	if err != nil || !ok || level == reasoning.Auto {
		return nil
	}
	if geminiUsesThinkingLevel(modelName) {
		mapped := geminiThinkingLevel(modelName, level)
		if mapped == "" {
			return nil
		}
		return &geminiThinkingConfig{ThinkingLevel: mapped}
	}
	budget, ok := geminiThinkingBudget(level)
	if !ok {
		return nil
	}
	return &geminiThinkingConfig{ThinkingBudget: &budget}
}

func thinkingAllowed(canThink []bool) bool {
	return len(canThink) == 0 || canThink[0]
}

func valueOfStringPtr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func geminiUsesThinkingLevel(modelName string) bool {
	m := strings.ToLower(strings.TrimSpace(modelName))
	return strings.Contains(m, "gemini-3")
}

func geminiThinkingLevel(modelName, level string) string {
	switch level {
	case reasoning.Off:
		if strings.Contains(strings.ToLower(modelName), "flash") {
			return "minimal"
		}
		return "low"
	case reasoning.Minimal:
		if strings.Contains(strings.ToLower(modelName), "flash") {
			return "minimal"
		}
		return "low"
	case reasoning.Low:
		return "low"
	case reasoning.Medium:
		if strings.Contains(strings.ToLower(modelName), "flash") {
			return "medium"
		}
		return "high"
	case reasoning.High, reasoning.XHigh:
		return "high"
	default:
		return ""
	}
}

func geminiThinkingBudget(level string) (int, bool) {
	switch level {
	case reasoning.Off:
		return 0, true
	case reasoning.Minimal, reasoning.Low:
		return 1024, true
	case reasoning.Medium:
		return 8192, true
	case reasoning.High:
		return 24576, true
	case reasoning.XHigh:
		return -1, true
	default:
		return 0, false
	}
}

// convert modelrepo messages to Gemini "contents"
func convertToGeminiMessages(messages []modelrepo.Message) []geminiContent {
	out := make([]geminiContent, 0, len(messages))

	// toolCallNameByID maps tool_call_id -> function name, needed to
	// populate FunctionResponse.Name for tool responses.
	toolCallNameByID := make(map[string]string)

	for _, m := range messages {
		// System messages are handled via SystemInstruction.
		if m.Role == "system" {
			continue
		}

		// Gemini roles are "user" | "model"; it does not accept "tool", so
		// tool responses are sent from the "user" side.
		var role string
		switch m.Role {
		case "assistant", "model":
			role = "model"
		default:
			role = "user"
		}

		parts := make([]geminiPart, 0)

		// Text goes first, so it's present even if tool calls/responses follow.
		if m.Content != "" && m.Role != "tool" {
			parts = append(parts, geminiPart{Text: m.Content})
		}

		// Image attachments: append one inlineData part per image, after the
		// text part, mirroring the OpenAI content-parts ordering. The resolver
		// only routes image-bearing requests to vision-capable providers.
		for _, img := range m.Images {
			parts = append(parts, geminiPart{
				InlineData: &geminiInlineData{
					MimeType: img.MimeType,
					Data:     img.Data,
				},
			})
		}

		// Assistant tool calls: encode as functionCall parts.
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" && tc.Function.Name != "" {
					toolCallNameByID[tc.ID] = tc.Function.Name
				}

				if tc.Function.Name == "" {
					continue
				}

				var args map[string]any
				if tc.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						args = map[string]any{}
					}
				} else {
					args = map[string]any{}
				}

				fc := &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: args,
				}

				// Gemini 3 requires thoughtSignature at the Part level: use the
				// preserved signature from ProviderMeta when available, else
				// the official Google bypass value so the strict validator
				// never rejects turns without one (first turn, parallel
				// calls, legacy history).
				part := geminiPart{FunctionCall: fc}
				if sig, ok := tc.ProviderMeta["thought_signature"]; ok && sig != "" {
					part.ThoughtSignature = sig
				} else {
					part.ThoughtSignature = "skip_thought_signature_validator"
				}

				parts = append(parts, part)
			}
		}

		// Tool responses: encode as functionResponse parts.
		if m.Role == "tool" {
			fnName := ""
			if m.ToolCallID != "" {
				if n, ok := toolCallNameByID[m.ToolCallID]; ok {
					fnName = n
				}
			}

			respMap := geminiToolResponseMap(m.Content)

			parts = append(parts, geminiPart{
				FunctionResponse: &geminiFunctionResponse{
					Name:     fnName,
					Response: respMap,
				},
			})
		}

		if len(parts) == 0 {
			continue
		}

		if len(out) > 0 && out[len(out)-1].Role == role {
			out[len(out)-1].Parts = append(out[len(out)-1].Parts, parts...)
		} else {
			out = append(out, geminiContent{
				Role:  role,
				Parts: parts,
			})
		}
	}

	return out
}

func geminiToolResponseMap(content string) map[string]interface{} {
	if content == "" {
		return map[string]interface{}{"content": ""}
	}

	var respData any
	if err := json.Unmarshal([]byte(content), &respData); err != nil {
		return map[string]interface{}{"content": content}
	}
	if geminiContainsSchemaReference(respData) {
		return map[string]interface{}{"content": content}
	}
	respMap, ok := respData.(map[string]any)
	if !ok {
		return map[string]interface{}{"content": respData}
	}
	return respMap
}

func geminiContainsSchemaReference(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch k {
			case "$schema", "$defs", "definitions":
				return true
			case "$ref":
				return true
			}
			if strings.HasPrefix(k, "$ref") {
				return true
			}
			if geminiContainsSchemaReference(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if geminiContainsSchemaReference(val) {
				return true
			}
		}
	}
	return false
}

// allowedGeminiSchemaFields lists the only JSON fields Gemini accepts in a Schema.
var allowedGeminiSchemaFields = map[string]bool{
	"type":        true,
	"description": true,
	"enum":        true,
	"items":       true,
	"properties":  true,
	"required":    true,
	"nullable":    true,
}

// sanitizeGeminiSchema transforms a JSON Schema map into a form compatible with Gemini.
// It converts "type" arrays to a single string, sets "nullable" when appropriate,
// and removes any unsupported fields.
func sanitizeGeminiSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}

	result := make(map[string]any)

	if typeVal, ok := schema["type"]; ok {
		switch v := typeVal.(type) {
		case string:
			result["type"] = v
		case []interface{}:
			// JSON Schema allows a type array (e.g. for nullable); Gemini
			// wants a single type string plus a separate nullable flag.
			var typeStr string
			nullable := false
			for _, elem := range v {
				if s, ok := elem.(string); ok {
					if s == "null" {
						nullable = true
						continue
					}
					// First non-null type wins as the primary type.
					if typeStr == "" {
						typeStr = s
					}
				}
			}
			if typeStr == "" {
				typeStr = "string"
			}
			result["type"] = typeStr
			if nullable {
				result["nullable"] = true
			}
		}
	}

	for _, field := range []string{"description", "enum", "required"} {
		if val, ok := schema[field]; ok {
			result[field] = val
		}
	}

	if items, ok := schema["items"]; ok {
		if itemsMap, ok := items.(map[string]any); ok {
			result["items"] = sanitizeGeminiSchema(itemsMap)
		}
	}

	if props, ok := schema["properties"]; ok {
		if propsMap, ok := props.(map[string]any); ok {
			cleanProps := make(map[string]any)
			for k, v := range propsMap {
				if subSchema, ok := v.(map[string]any); ok {
					cleanProps[k] = sanitizeGeminiSchema(subSchema)
				} else {
					cleanProps[k] = v // not a map (unlikely); keep as-is
				}
			}
			result["properties"] = cleanProps
		}
	}

	// Preserve a pre-existing nullable if sanitization didn't set one.
	if nullable, ok := schema["nullable"]; ok && nullable != nil {
		if _, exists := result["nullable"]; !exists {
			result["nullable"] = nullable
		}
	}

	return result
}

// geminiSanitiseSchema converts arbitrary JSON Schema to Gemini's exact schema format.
func geminiSanitiseSchema(params any) (*geminiSchema, error) {
	if params == nil {
		return nil, nil
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	var schemaMap map[string]any
	if err := json.Unmarshal(raw, &schemaMap); err != nil {
		// Not an object (e.g. a primitive): treat as empty rather than erroring.
		schemaMap = make(map[string]any)
	}

	cleanedMap := sanitizeGeminiSchema(schemaMap)

	cleanedRaw, err := json.Marshal(cleanedMap)
	if err != nil {
		return nil, err
	}
	var schema geminiSchema
	if err := json.Unmarshal(cleanedRaw, &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}
