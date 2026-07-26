package bedrock

import (
	"context"
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
)

type bedrockPromptClient struct{ bedrockClient }

// Prompt implements modelrepo.LLMPromptExecClient by wrapping Chat.
func (c *bedrockPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	msgs := []modelrepo.Message{{Role: "user", Content: prompt}}
	if s := strings.TrimSpace(systemInstruction); s != "" {
		msgs = append([]modelrepo.Message{{Role: "system", Content: s}}, msgs...)
	}
	chat := &bedrockChatClient{c.bedrockClient}
	res, err := chat.Chat(ctx, msgs, modelrepo.WithTemperature(float64(temperature)))
	if err != nil {
		return "", err
	}
	return res.Message.Content, nil
}

var _ modelrepo.LLMPromptExecClient = (*bedrockPromptClient)(nil)
