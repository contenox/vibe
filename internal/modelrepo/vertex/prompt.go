package vertex

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
)

type vertexPromptClient struct {
	vertexClient
}

// Prompt implements modelrepo.LLMPromptExecClient.
func (c *vertexPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "prompt", "vertex", "model", c.modelName)
	defer end()

	messages := []modelrepo.Message{
		{Role: "user", Content: prompt},
	}
	if s := strings.TrimSpace(systemInstruction); s != "" {
		messages = append([]modelrepo.Message{{Role: "system", Content: s}}, messages...)
	}

	chat := &vertexChatClient{vertexClient: c.vertexClient}
	resp, err := chat.Chat(ctx, messages, modelrepo.WithTemperature(float64(temperature)))
	if err != nil {
		reportErr(err)
		return "", fmt.Errorf("vertex prompt execution failed: %w", err)
	}

	reportChange("prompt_completed", map[string]any{
		"response_length": len(resp.Message.Content),
	})
	return resp.Message.Content, nil
}

var _ modelrepo.LLMPromptExecClient = (*vertexPromptClient)(nil)
