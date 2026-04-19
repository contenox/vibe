package gemini

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

type geminiClient struct {
	apiKey     string
	modelName  string
	baseURL    string
	httpClient *http.Client
	maxTokens  int
	tracker    libtracker.ActivityTracker
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

// geminiThinkingConfig maps to Gemini's thinkingConfig.thinkingBudget:
//
//	 -1 = dynamic / uncapped
//	  0 = disabled
//	N>0 = exact token budget
type geminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

// sendRequest: shared HTTP helper for Gemini clients
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
	req.Header.Set("X-Goog-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		err = fmt.Errorf("HTTP request failed for model %s: %w", c.modelName, err)
		reportErr(err)
		return err
	}
	defer resp.Body.Close()

	// Log headers via tracker
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
		body, _ := io.ReadAll(resp.Body)
		if jsonErr := json.Unmarshal(body, &eresp); jsonErr == nil && eresp.Error.Message != "" {
			err = fmt.Errorf("gemini API error: %d %s - %s (model=%s url=%s)",
				resp.StatusCode, eresp.Error.Status, eresp.Error.Message, c.modelName, fullURL)
			reportErr(err)
			return err
		}
		err = fmt.Errorf("gemini API error: %d - %s (model=%s url=%s)", resp.StatusCode, string(body), c.modelName, fullURL)
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

// buildGeminiRequest builds a proper Gemini generateContent request using modelrepo args & tools
func buildGeminiRequest(_ string, messages []modelrepo.Message, systemInstruction *geminiSystemInstruction, args []modelrepo.ChatArgument) (geminiGenerateContentRequest, error) {
	// Collect chat args
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}

	// Convert tools -> Gemini tool declarations
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

	req := geminiGenerateContentRequest{
		SystemInstruction: systemInstruction,
		Contents:          convertToGeminiMessages(messages),
		GenerationConfig:  &geminiGenerationConfig{},
		Tools:             tools,
	}
	req.GenerationConfig.Temperature = cfg.Temperature
	req.GenerationConfig.TopP = cfg.TopP
	if cfg.MaxTokens != nil {
		req.GenerationConfig.MaxOutputTokens = cfg.MaxTokens
	}
	req.GenerationConfig.Seed = cfg.Seed

	// Wire ThinkingConfig for Gemini 2.5+ thinking models.
	// Omitting it (nil) means the model uses its default (usually no thinking).
	if cfg.Think != nil {
		switch *cfg.Think {
		case "true", "high":
			req.GenerationConfig.ThinkingConfig = &geminiThinkingConfig{ThinkingBudget: -1} // uncapped
		case "medium":
			req.GenerationConfig.ThinkingConfig = &geminiThinkingConfig{ThinkingBudget: 8192}
		case "low":
			req.GenerationConfig.ThinkingConfig = &geminiThinkingConfig{ThinkingBudget: 1024}
		case "false":
			req.GenerationConfig.ThinkingConfig = &geminiThinkingConfig{ThinkingBudget: 0}
		}
	}

	return req, nil
}

// convert modelrepo messages to Gemini "contents"
func convertToGeminiMessages(messages []modelrepo.Message) []geminiContent {
	out := make([]geminiContent, 0, len(messages))

	// Map OpenAI-style tool_call_id -> function name so we can
	// populate FunctionResponse.Name for tool responses.
	toolCallNameByID := make(map[string]string)

	for _, m := range messages {
		// System messages are handled via SystemInstruction
		if m.Role == "system" {
			continue
		}

		// Map internal roles to Gemini roles ("user" | "model").
		// Gemini does NOT accept "tool", so we treat tool responses
		// as coming from the "user" side.
		var role string
		switch m.Role {
		case "assistant", "model":
			// provider-agnostic "assistant" -> Gemini "model"
			role = "model"
		default:
			// "user", "tool", or anything else -> "user"
			role = "user"
		}

		parts := make([]geminiPart, 0)

		// Add text part FIRST if it's not a tool role ---
		// Handle text content for user/assistant messages.
		// This ensures text is included even if tool calls/responses follow.
		if m.Content != "" && m.Role != "tool" {
			parts = append(parts, geminiPart{Text: m.Content})
		}

		// Assistant tool calls: encode as functionCall parts
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				// Remember mapping from tool_call_id -> function name
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

				// Gemini 3 requires thoughtSignature at the Part level.
				// Use the preserved signature from ProviderMeta when available.
				// For tool calls without a signature (first turn, parallel calls, legacy history)
				// use the official Google bypass value so the strict validator never rejects the turn.
				// See: https://ai.google.dev/gemini-api/docs/thought-signatures
				part := geminiPart{FunctionCall: fc}
				if sig, ok := tc.ProviderMeta["thought_signature"]; ok && sig != "" {
					part.ThoughtSignature = sig
				} else {
					part.ThoughtSignature = "skip_thought_signature_validator"
				}

				parts = append(parts, part)
			}
		}

		// Properly handle tool responses
		// Tool responses: encode as functionResponse parts if we can
		if m.Role == "tool" {
			// Try to find the function name associated with this tool_call_id
			fnName := ""
			if m.ToolCallID != "" {
				if n, ok := toolCallNameByID[m.ToolCallID]; ok {
					fnName = n
				}
			}

			var respData any
			if m.Content != "" {
				// Try to unmarshal as *any* valid JSON
				if err := json.Unmarshal([]byte(m.Content), &respData); err != nil {
					// If it's not JSON (e.g., "it's sunny"),
					// treat the raw string as the content.
					respData = m.Content
				}
			} else {
				// Empty content
				respData = ""
			}

			// Gemini's FunctionResponse.Response *must* be a JSON object.
			// If our data isn't one, wrap it.
			respMap, ok := respData.(map[string]any)
			if !ok {
				// It wasn't a JSON object (e.g., string, number, array, or null).
				// Wrap it in a standard object.
				respMap = map[string]any{"content": respData}
			}

			parts = append(parts, geminiPart{
				FunctionResponse: &geminiFunctionResponse{
					Name:     fnName,
					Response: respMap,
				},
			})
		}

		// 3) Normal text content (user/assistant/system-like)
		// If we somehow ended up with no parts at all, skip this message
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

	// Handle "type" field specially
	if typeVal, ok := schema["type"]; ok {
		switch v := typeVal.(type) {
		case string:
			result["type"] = v
		case []interface{}:
			// Convert array to string, and optionally set nullable
			var typeStr string
			nullable := false
			for _, elem := range v {
				if s, ok := elem.(string); ok {
					if s == "null" {
						nullable = true
						continue
					}
					// Take first non-null type as the primary type
					if typeStr == "" {
						typeStr = s
					}
				}
			}
			if typeStr == "" {
				typeStr = "string" // fallback
			}
			result["type"] = typeStr
			if nullable {
				result["nullable"] = true
			}
		}
	}

	// Handle other allowed fields
	for _, field := range []string{"description", "enum", "required"} {
		if val, ok := schema[field]; ok {
			result[field] = val
		}
	}

	// Recursively handle "items"
	if items, ok := schema["items"]; ok {
		if itemsMap, ok := items.(map[string]any); ok {
			result["items"] = sanitizeGeminiSchema(itemsMap)
		}
	}

	// Recursively handle "properties"
	if props, ok := schema["properties"]; ok {
		if propsMap, ok := props.(map[string]any); ok {
			cleanProps := make(map[string]any)
			for k, v := range propsMap {
				if subSchema, ok := v.(map[string]any); ok {
					cleanProps[k] = sanitizeGeminiSchema(subSchema)
				} else {
					// If not a map, keep as is (unlikely)
					cleanProps[k] = v
				}
			}
			result["properties"] = cleanProps
		}
	}

	// "nullable" may already be set, but if it wasn't, we don't add it.
	// If it was set originally and not overwritten, preserve it.
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

	// Step 1: Marshal to JSON to get a clean representation.
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	// Step 2: Unmarshal into a map to manipulate.
	var schemaMap map[string]any
	if err := json.Unmarshal(raw, &schemaMap); err != nil {
		// If it's not an object (e.g., a primitive), we can't do much; treat as empty.
		schemaMap = make(map[string]any)
	}

	// Step 3: Recursively sanitize.
	cleanedMap := sanitizeGeminiSchema(schemaMap)

	// Step 4: Marshal back to JSON and then unmarshal into the strict struct.
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
