package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/contenox/contenox/internal/modelrepo"
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

	req, err := buildGeminiRequest(c.modelName, filtered, systemInstruction, args)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	endpoint := fmt.Sprintf("/v1beta/models/%s:generateContent", c.modelName)
	var resp geminiGenerateContentResponse
	if err := c.sendRequest(ctx, endpoint, req, &resp); err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	if len(resp.Candidates) == 0 {
		reason := resp.PromptFeedback.BlockReason
		if reason == "" {
			reason = "unknown (check safety filters)"
		}
		err := fmt.Errorf("no candidates returned from Gemini for model %s: prompt blocked (%s)", c.modelName, reason)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	cand := resp.Candidates[0]
	if len(cand.Content.Parts) == 0 {
		reason := cand.FinishReason
		if reason == "" {
			reason = "unknown"
		}
		err := fmt.Errorf("empty candidate parts from Gemini for model %s: finish reason (%s)", c.modelName, reason)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	var (
		outText       string
		thinkingText  string
		toolCalls     []modelrepo.ToolCall
		lastSignature string
	)
	for _, p := range cand.Content.Parts {
		switch {
		case p.Thought && p.Text != "":
			// Gemini 2.5+ returns thinking content in Parts with thought=true
			thinkingText += p.Text
		case p.Text != "":
			outText += p.Text
		case p.FunctionCall != nil:
			// Convert args (map[string]any) -> JSON string
			argsJSON, err := json.Marshal(p.FunctionCall.Args)
			if err != nil {
				continue
			}
			id := fmt.Sprintf("%x", rand.Int63())
			tc := modelrepo.ToolCall{
				ID:   id,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      p.FunctionCall.Name,
					Arguments: string(argsJSON),
				},
			}
			// Gemini 3: thoughtSignature is at the Part level (p.ThoughtSignature),
			// not inside functionCall. Propagate it to parallel calls in the same turn
			// where Gemini may only set it once.
			sig := p.ThoughtSignature
			if sig == "" {
				sig = p.FunctionCall.ThoughtSignature // fallback: older API placement
			}
			if sig == "" {
				sig = lastSignature // propagate to parallel calls in same turn
			}
			if sig != "" {
				lastSignature = sig
				tc.ProviderMeta = map[string]string{"thought_signature": sig}
			}
			toolCalls = append(toolCalls, tc)
		}
	}

	if outText == "" && len(toolCalls) == 0 {
		err := fmt.Errorf("empty content from model %s", c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	result := modelrepo.ChatResult{
		Message:   modelrepo.Message{Role: "assistant", Content: outText, Thinking: thinkingText},
		ToolCalls: toolCalls,
	}

	reportChange("chat_completed", result)
	return result, nil
}

var _ modelrepo.LLMChatClient = (*GeminiChatClient)(nil)
