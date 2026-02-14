package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/contenox/vibe/internal/modelrepo"
)

type GeminiChatClient struct {
	geminiClient
}

// Chat implements modelrepo.LLMChatClient
func (c *GeminiChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "gemini", "model", c.modelName)
	defer end()

	// Pull out an optional system instruction
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

	req := buildGeminiRequest(c.modelName, filtered, systemInstruction, args)

	endpoint := fmt.Sprintf("/v1beta/models/%s:generateContent", c.modelName)
	var resp geminiGenerateContentResponse
	if err := c.sendRequest(ctx, endpoint, req, &resp); err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	if len(resp.Candidates) == 0 {
		err := fmt.Errorf("no candidates returned from Gemini for model %s", c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	cand := resp.Candidates[0]

	var (
		outText   string
		toolCalls []modelrepo.ToolCall
	)

	for _, p := range cand.Content.Parts {
		switch {
		case p.Text != "":
			outText += p.Text
		case p.FunctionCall != nil:
			// Convert args (map[string]any) -> JSON string
			argsJSON, err := json.Marshal(p.FunctionCall.Args)
			if err != nil {
				// Skip this call but still try to return text/tool calls we did parse
				continue
			}
			id := fmt.Sprintf("%x", rand.Int63())
			toolCalls = append(toolCalls, modelrepo.ToolCall{
				ID:   id,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      p.FunctionCall.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	if outText == "" && len(toolCalls) == 0 {
		err := fmt.Errorf("empty content from model %s", c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	result := modelrepo.ChatResult{
		Message:   modelrepo.Message{Role: "assistant", Content: outText},
		ToolCalls: toolCalls,
	}

	reportChange("chat_completed", result)
	return result, nil
}

var _ modelrepo.LLMChatClient = (*GeminiChatClient)(nil)
