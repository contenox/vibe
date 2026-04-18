package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type vertexClient struct {
	baseURL       string
	publisher     string
	modelName     string
	contextLength int
	credJSON      string // service account JSON; empty → ADC
	httpClient    *http.Client
	tracker       libtracker.ActivityTracker
	tokenFn       func(context.Context) (string, error) // test hook; overrides credJSON when set
}

// endpoint builds the full Vertex AI API URL for a given method (e.g. "generateContent").
func (c *vertexClient) endpoint(method string) string {
	return strings.TrimRight(c.baseURL, "/") +
		"/publishers/" + c.publisher +
		"/models/" + c.modelName +
		":" + method
}

// sendRequest POSTs to the given Vertex AI endpoint with ADC bearer auth.
// Pattern mirrors gemini/client.go sendRequest.
func (c *vertexClient) sendRequest(ctx context.Context, endpoint string, request interface{}, response interface{}) error {
	fullURL := endpoint

	reportErr, reportChange, end := c.tracker.Start(
		ctx,
		"http_request",
		"vertex",
		"model", c.modelName,
		"publisher", c.publisher,
		"endpoint", endpoint,
	)
	defer end()

	var reqBody io.Reader
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			err = fmt.Errorf("failed to marshal request: %w", err)
			reportErr(err)
			return err
		}
		reqBody = bytes.NewBuffer(b)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, reqBody)
	if err != nil {
		err = fmt.Errorf("failed to create request: %w", err)
		reportErr(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	tokenFn := c.tokenFn
	if tokenFn == nil {
		tokenFn = func(ctx context.Context) (string, error) {
			return BearerTokenWithCreds(ctx, c.credJSON)
		}
	}
	token, err := tokenFn(ctx)
	if err != nil {
		reportErr(err)
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
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
		body, _ := io.ReadAll(resp.Body)
		var eresp vertexErrorResponse
		if jsonErr := json.Unmarshal(body, &eresp); jsonErr == nil && eresp.Error.Message != "" {
			err = fmt.Errorf("vertex API error: %d %s - %s (model=%s url=%s)",
				resp.StatusCode, eresp.Error.Status, eresp.Error.Message, c.modelName, fullURL)
			reportErr(err)
			return err
		}
		err = fmt.Errorf("vertex API error: %d - %s (model=%s url=%s)", resp.StatusCode, string(body), c.modelName, fullURL)
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

// buildVertexRequest converts modelrepo messages and args to a vertexRequest.
// Free function matching OpenAI/Gemini convention.
func buildVertexRequest(messages []modelrepo.Message, args []modelrepo.ChatArgument) (vertexRequest, error) {
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

	req := vertexRequest{
		SystemInstruction: systemInstruction,
		Contents:          convertToVertexContents(filtered),
		GenerationConfig:  &vertexGenerationConfig{},
		Tools:             tools,
	}
	req.GenerationConfig.Temperature = cfg.Temperature
	req.GenerationConfig.TopP = cfg.TopP
	req.GenerationConfig.MaxOutputTokens = cfg.MaxTokens
	req.GenerationConfig.Seed = cfg.Seed

	return req, nil
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
				parts = append(parts, vertexPart{
					FunctionCall: &vertexFunctionCall{Name: tc.Function.Name, Args: args},
				})
			}
		}

		if m.Role == "tool" {
			fnName := ""
			if m.ToolCallID != "" {
				if n, ok := toolCallNameByID[m.ToolCallID]; ok {
					fnName = n
				}
			}
			var respData any
			if m.Content != "" {
				if err := json.Unmarshal([]byte(m.Content), &respData); err != nil {
					respData = m.Content
				}
			} else {
				respData = ""
			}
			respMap, ok := respData.(map[string]any)
			if !ok {
				respMap = map[string]any{"content": respData}
			}
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
			for _, elem := range v {
				if s, ok := elem.(string); ok {
					if s == "null" {
						nullable = true
						continue
					}
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
