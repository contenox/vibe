package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type vertexClient struct {
	baseURL         string
	publisher       string
	modelName       string
	contextLength   int
	maxOutputTokens int
	credJSON        string // service account JSON; empty → ADC
	httpClient      *http.Client
	canThink        bool
	tracker         libtracker.ActivityTracker
	tokenFn         func(context.Context) (string, error) // test tools; overrides credJSON when set
}

// endpoint builds the full Vertex AI API URL for a given method (e.g. "generateContent").
func (c *vertexClient) endpoint(method string) string {
	return strings.TrimRight(c.baseURL, "/") +
		"/publishers/" + c.publisher +
		"/models/" + c.modelName +
		":" + method
}

// bearer returns an OAuth2 access token using the provider's cached source
// (the per-call BearerTokenWithCreds fallback only fires in tests, where
// tokenFn is unset).
func (c *vertexClient) bearer(ctx context.Context) (string, error) {
	tokenFn := c.tokenFn
	if tokenFn == nil {
		tokenFn = func(ctx context.Context) (string, error) {
			return BearerTokenWithCreds(ctx, c.credJSON)
		}
	}
	return tokenFn(ctx)
}

// authHeaders sets the bearer token and the quota-project header on req.
func (c *vertexClient) authHeaders(ctx context.Context, req *http.Request) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if project := extractProjectFromVertexURL(c.baseURL); project != "" {
		req.Header.Set("x-goog-user-project", project)
	}
	return nil
}

// postJSON POSTs request as JSON to endpoint with ADC bearer auth and returns
// the raw response body. Used by the Gemini path via sendRequest.
func (c *vertexClient) postJSON(ctx context.Context, endpoint string, request any) ([]byte, error) {
	reportErr, reportChange, end := c.tracker.Start(
		ctx,
		"http_request",
		"vertex",
		"model", c.modelName,
		"publisher", c.publisher,
		"endpoint", endpoint,
	)
	defer end()

	var payload []byte
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			err = fmt.Errorf("failed to marshal request: %w", err)
			reportErr(err)
			return nil, err
		}
		payload = b
	}

	// Non-streaming call: bounded end-to-end, retried on 429/5xx with
	// Retry-After honored.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()
	resp, err := modelrepo.DoWithRetry(ctx, c.httpClient, func() (*http.Request, error) {
		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if err := c.authHeaders(ctx, req); err != nil {
			return nil, err
		}
		return req, nil
	})
	if err != nil {
		err = fmt.Errorf("HTTP request failed for model %s: %w", c.modelName, err)
		reportErr(err)
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	reportChange("http_response", map[string]any{
		"status_code": resp.StatusCode,
		"headers":     resp.Header,
	})

	if resp.StatusCode != http.StatusOK {
		var eresp vertexErrorResponse
		if jsonErr := json.Unmarshal(body, &eresp); jsonErr == nil && eresp.Error.Message != "" {
			// Same documented shapes as Gemini: RESOURCE_EXHAUSTED → rate
			// limit, INVALID_ARGUMENT with token phrasing → context overflow.
			err = fmt.Errorf("vertex API error: %d %s - %s (model=%s url=%s)",
				resp.StatusCode, eresp.Error.Status, eresp.Error.Message, c.modelName, endpoint)
			err = modelrepo.ClassifyProviderError(err, resp.StatusCode, eresp.Error.Status, eresp.Error.Message)
			reportErr(err)
			return nil, err
		}
		err = fmt.Errorf("vertex API error: %d - %s (model=%s url=%s)", resp.StatusCode, string(body), c.modelName, endpoint)
		err = modelrepo.ClassifyProviderError(err, resp.StatusCode, "", string(body))
		reportErr(err)
		return nil, err
	}

	reportChange("request_completed", nil)
	return body, nil
}

// sendRequest POSTs to the given Vertex AI endpoint and decodes the JSON
// response into response. Pattern mirrors gemini/client.go sendRequest.
func (c *vertexClient) sendRequest(ctx context.Context, endpoint string, request interface{}, response interface{}) error {
	body, err := c.postJSON(ctx, endpoint, request)
	if err != nil {
		return err
	}
	if response != nil {
		if err := json.Unmarshal(body, response); err != nil {
			return fmt.Errorf("failed to decode response for model %s: %w", c.modelName, err)
		}
	}
	return nil
}

// buildVertexRequest converts modelrepo messages and args to a vertexRequest.
// Free function matching OpenAI/Gemini convention.
func buildVertexRequest(modelName string, messages []modelrepo.Message, args []modelrepo.ChatArgument, canThink ...bool) (vertexRequest, error) {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}

	var systemInstruction *vertexContent
	filtered := make([]modelrepo.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if m.Content != "" {
				systemInstruction = &vertexContent{
					Parts: []vertexPart{{Text: m.Content}},
				}
			}
			continue
		}
		filtered = append(filtered, m)
	}

	var tools []vertexToolRequest
	if len(cfg.Tools) > 0 {
		decls := make([]vertexFunctionDeclaration, 0, len(cfg.Tools))
		for _, t := range cfg.Tools {
			if t.Type != "function" || t.Function == nil {
				continue
			}
			schema, err := sanitizeVertexSchema(t.Function.Parameters)
			if err != nil {
				return vertexRequest{}, err
			}
			decls = append(decls, vertexFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  schema,
			})
		}
		if len(decls) > 0 {
			tools = append(tools, vertexToolRequest{FunctionDeclarations: decls})
		}
	}

	contents := convertToVertexContents(filtered)
	if len(contents) == 0 {
		return vertexRequest{}, fmt.Errorf("vertex: refusing to send empty contents for model %s after filtering %d message(s); provide at least one non-empty user/model/tool message, tool call, or tool response", modelName, len(messages))
	}

	req := vertexRequest{
		SystemInstruction: systemInstruction,
		Contents:          contents,
		GenerationConfig:  &vertexGenerationConfig{},
		Tools:             tools,
	}
	req.GenerationConfig.Temperature = cfg.Temperature
	req.GenerationConfig.TopP = cfg.TopP
	req.GenerationConfig.MaxOutputTokens = cfg.MaxTokens
	req.GenerationConfig.Seed = cfg.Seed
	if vertexThinkingAllowed(canThink) {
		req.GenerationConfig.ThinkingConfig = vertexThinkingConfigForModel(modelName, cfg.Think)
	}

	return req, nil
}

func vertexThinkingAllowed(canThink []bool) bool {
	return len(canThink) == 0 || canThink[0]
}

func vertexThinkingConfigForModel(modelName string, think *string) *vertexThinkingConfig {
	level, ok, err := reasoning.NormalizeOptional(vertexStringPtrValue(think))
	if err != nil || !ok || level == reasoning.Auto {
		return nil
	}
	if vertexUsesThinkingLevel(modelName) {
		mapped := vertexThinkingLevel(modelName, level)
		if mapped == "" {
			return nil
		}
		return &vertexThinkingConfig{ThinkingLevel: mapped}
	}
	budget, ok := vertexThinkingBudget(level)
	if !ok {
		return nil
	}
	return &vertexThinkingConfig{ThinkingBudget: &budget}
}

func vertexStringPtrValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func vertexUsesThinkingLevel(modelName string) bool {
	m := strings.ToLower(strings.TrimSpace(modelName))
	return strings.Contains(m, "gemini-3")
}

func vertexThinkingLevel(modelName, level string) string {
	m := strings.ToLower(modelName)
	switch level {
	case reasoning.Off:
		if strings.Contains(m, "flash") {
			return "minimal"
		}
		return "low"
	case reasoning.Minimal:
		if strings.Contains(m, "flash") {
			return "minimal"
		}
		return "low"
	case reasoning.Low:
		return "low"
	case reasoning.Medium:
		if strings.Contains(m, "flash") {
			return "medium"
		}
		return "high"
	case reasoning.High, reasoning.XHigh:
		return "high"
	default:
		return ""
	}
}

func vertexThinkingBudget(level string) (int, bool) {
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

// convertToVertexContents maps modelrepo messages to Vertex AI content format.
// Mirrors convertToGeminiMessages in the gemini package.
func convertToVertexContents(messages []modelrepo.Message) []vertexContent {
	out := make([]vertexContent, 0, len(messages))
	toolCallNameByID := make(map[string]string)

	for _, m := range messages {
		if m.Role == "system" {
			continue
		}

		var role string
		switch m.Role {
		case "assistant", "model":
			role = "model"
		default:
			role = "user"
		}

		parts := make([]vertexPart, 0)

		if m.Content != "" && m.Role != "tool" {
			parts = append(parts, vertexPart{Text: m.Content})
		}

		// Image attachments: append one inlineData part per image, after the
		// text part, mirroring the OpenAI content-parts ordering. The resolver
		// only routes image-bearing requests to vision-capable providers.
		for _, img := range m.Images {
			parts = append(parts, vertexPart{
				InlineData: &vertexInlineData{
					MimeType: img.MimeType,
					Data:     img.Data,
				},
			})
		}

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
				part := vertexPart{
					FunctionCall: &vertexFunctionCall{Name: tc.Function.Name, Args: args},
				}
				if sig, ok := tc.ProviderMeta["thought_signature"]; ok && sig != "" {
					part.ThoughtSignature = sig
				} else {
					part.ThoughtSignature = "skip_thought_signature_validator"
				}
				parts = append(parts, part)
			}
		}

		if m.Role == "tool" {
			fnName := "tool_response"
			if m.ToolCallID != "" {
				if n, ok := toolCallNameByID[m.ToolCallID]; ok {
					fnName = n
				}
			}
			respMap := vertexToolResponseMap(m.Content)
			parts = append(parts, vertexPart{
				FunctionResponse: &vertexFunctionResponse{Name: fnName, Response: respMap},
			})
		}

		if len(parts) == 0 {
			continue
		}

		if len(out) > 0 && out[len(out)-1].Role == role {
			out[len(out)-1].Parts = append(out[len(out)-1].Parts, parts...)
		} else {
			out = append(out, vertexContent{Role: role, Parts: parts})
		}
	}

	return out
}

func vertexToolResponseMap(content string) map[string]interface{} {
	if content == "" {
		return map[string]interface{}{"content": ""}
	}

	var respData any
	if err := json.Unmarshal([]byte(content), &respData); err != nil {
		return map[string]interface{}{"content": content}
	}
	if vertexContainsSchemaReference(respData) {
		return map[string]interface{}{"content": content}
	}
	respMap, ok := respData.(map[string]any)
	if !ok {
		return map[string]interface{}{"content": respData}
	}
	return respMap
}

func vertexContainsSchemaReference(v any) bool {
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
			if vertexContainsSchemaReference(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if vertexContainsSchemaReference(val) {
				return true
			}
		}
	}
	return false
}

// sanitizeVertexSchema converts arbitrary JSON Schema to Vertex AI's accepted format.
// Vertex AI uses the same schema constraints as Gemini AI Studio.
func sanitizeVertexSchema(params any) (*vertexSchema, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var schemaMap map[string]any
	if err := json.Unmarshal(raw, &schemaMap); err != nil {
		schemaMap = make(map[string]any)
	}
	cleaned := sanitizeSchemaMap(schemaMap)
	cleanedRaw, err := json.Marshal(cleaned)
	if err != nil {
		return nil, err
	}
	var schema vertexSchema
	if err := json.Unmarshal(cleanedRaw, &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

func sanitizeSchemaMap(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	result := make(map[string]any)

	if typeVal, ok := schema["type"]; ok {
		switch v := typeVal.(type) {
		case string:
			result["type"] = v
		case []interface{}:
			var typeStr string
			nullable := false
			unionHasArray := false
			for _, elem := range v {
				if s, ok := elem.(string); ok {
					if s == "null" {
						nullable = true
						continue
					}
					if s == "array" {
						unionHasArray = true
					}
					if typeStr == "" {
						typeStr = s
					}
				}
			}
			if typeStr == "" {
				typeStr = "string"
			}
			// A union carrying its own items ("one path, or an array of them")
			// must collapse to the ARRAY branch, not merely the first one:
			// Vertex rejects a declaration whose schema has items while its
			// type is anything else. Widening to array loses nothing, because
			// every such argument's Go side already accepts a one-element list.
			if _, hasItems := schema["items"]; hasItems && unionHasArray {
				typeStr = "array"
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

	// items rides along ONLY on an array. Vertex refuses the whole
	// functionDeclaration when it sees items under any other type, which takes
	// down every tool in the request rather than the one bad property — so a
	// stray items is dropped here instead of forwarded.
	if items, ok := schema["items"]; ok && result["type"] == "array" {
		if itemsMap, ok := items.(map[string]any); ok {
			result["items"] = sanitizeSchemaMap(itemsMap)
		}
	}

	if props, ok := schema["properties"]; ok {
		if propsMap, ok := props.(map[string]any); ok {
			cleanProps := make(map[string]any)
			for k, v := range propsMap {
				if subSchema, ok := v.(map[string]any); ok {
					cleanProps[k] = sanitizeSchemaMap(subSchema)
				} else {
					cleanProps[k] = v
				}
			}
			result["properties"] = cleanProps
		}
	}

	if nullable, ok := schema["nullable"]; ok && nullable != nil {
		if _, exists := result["nullable"]; !exists {
			result["nullable"] = nullable
		}
	}

	return result
}
