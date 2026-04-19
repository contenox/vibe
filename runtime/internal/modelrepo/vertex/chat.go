package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
)

type vertexChatClient struct {
	vertexClient
}

// Chat implements modelrepo.LLMChatClient.
func (c *vertexChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "vertex", "model", c.modelName)
	defer end()

	req, err := buildVertexRequest(messages, args)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	var resp vertexResponse
	if err := c.sendRequest(ctx, c.endpoint("generateContent"), req, &resp); err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	if len(resp.Candidates) == 0 {
		reason := resp.PromptFeedback.BlockReason
		if reason == "" {
			reason = "unknown (check safety filters)"
		}
		err := fmt.Errorf("no candidates returned from Vertex AI for model %s: prompt blocked (%s)", c.modelName, reason)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	cand := resp.Candidates[0]
	if len(cand.Content.Parts) == 0 {
		reason := cand.FinishReason
		if reason == "" {
			reason = "unknown"
		}
		err := fmt.Errorf("empty candidate parts from Vertex AI for model %s: finish reason (%s)", c.modelName, reason)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	var outText, thinkingText string
	var toolCalls []modelrepo.ToolCall
	for _, p := range cand.Content.Parts {
		switch {
		case p.Thought && p.Text != "":
			thinkingText += p.Text
		case p.Text != "":
			outText += p.Text
		case p.FunctionCall != nil:
			argsJSON, err := json.Marshal(p.FunctionCall.Args)
			if err != nil {
				continue
			}
			toolCalls = append(toolCalls, modelrepo.ToolCall{
				ID:   fmt.Sprintf("%x", rand.Int63()),
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
		err := fmt.Errorf("empty content from Vertex AI model %s", c.modelName)
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

var _ modelrepo.LLMChatClient = (*vertexChatClient)(nil)
