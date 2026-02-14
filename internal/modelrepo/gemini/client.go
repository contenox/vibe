package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
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
func buildGeminiRequest(_ string, messages []modelrepo.Message, systemInstruction *geminiSystemInstruction, args []modelrepo.ChatArgument) geminiGenerateContentRequest {
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
			if t.Type == "function" && t.Function != nil {
				decls = append(decls, geminiFunctionDeclaration{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
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

	return req
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

		// --- FIX 1: Add text part FIRST if it's not a tool role ---
		// Handle text content for user/assistant messages.
		// This ensures text is included even if tool calls/responses follow.
		if m.Content != "" && m.Role != "tool" {
			parts = append(parts, geminiPart{Text: m.Content})
		}
		// --- END FIX 1 ---

		// 1) Assistant tool calls: encode as functionCall parts
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
						// If args aren't valid JSON, fall back to empty map
						args = map[string]any{}
					}
				} else {
					args = map[string]any{}
				}

				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Function.Name,
						Args: args,
					},
				})
			}
		}

		// --- FIX 2: Properly handle tool responses ---
		// 2) Tool responses: encode as functionResponse parts if we can
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
		// --- END FIX 2 ---

		// 3) Normal text content (user/assistant/system-like)
		// -- THIS BLOCK IS NOW REMOVED and handled by FIX 1 --
		// if m.Content != "" && len(parts) == 0 {
		// 	parts = append(parts, geminiPart{Text: m.Content})
		// }

		// If we somehow ended up with no parts at all, skip this message
		if len(parts) == 0 {
			continue
		}

		out = append(out, geminiContent{
			Role:  role,
			Parts: parts,
		})
	}

	return out
}
