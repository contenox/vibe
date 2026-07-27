package bedrock

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/models/modelrepo"
)

type bedrockChatClient struct{ bedrockClient }

// Chat implements modelrepo.LLMChatClient via the Bedrock Converse API.
func (c *bedrockChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "bedrock", "model", c.modelName)
	defer end()

	in, toOriginal, err := buildConverseInput(c.modelName, messages, chatConfigFromArgs(args), c.maxOutputTokens)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	out, err := c.api.Converse(ctx, in)
	if err != nil {
		err = classifyBedrockError(fmt.Errorf("bedrock converse (model=%s): %w", c.modelName, err))
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	res, err := decodeConverse(out, toOriginal)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	reportChange("chat_completed", res)
	return res, nil
}

var _ modelrepo.LLMChatClient = (*bedrockChatClient)(nil)
