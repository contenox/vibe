package openai

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/models/modelrepo"
)

type OpenAIPromptClient struct {
	openAIClient
}

func (c *OpenAIPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *modelrepo.TokenUsage, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "prompt", "openai", "model", c.modelName)
	defer end()

	messages := []modelrepo.Message{
		{Role: "system", Content: systemInstruction},
		{Role: "user", Content: prompt},
	}

	// Legacy GPT-5 chat completions reject sampling params; newer GPT-5.x
	// snapshots may allow them under reasoning=none.
	var args []modelrepo.ChatArgument
	if !openAIShouldOmitSamplingParams(c.modelName, "") {
		args = append(args, modelrepo.WithTemperature(float64(temperature)))
	}

	chatClient := &OpenAIChatClient{openAIClient: c.openAIClient}
	resp, err := chatClient.Chat(ctx, messages, args...)
	if err != nil {
		reportErr(err)
		return "", nil, fmt.Errorf("OpenAI prompt execution failed: %w", err)
	}

	reportChange("prompt_completed", map[string]any{
		"response_length": len(resp.Message.Content),
	})
	return resp.Message.Content, resp.Usage, nil
}

func (c *OpenAIPromptClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.Message, error) {
	chatClient := &OpenAIChatClient{openAIClient: c.openAIClient}
	resp, err := chatClient.Chat(ctx, messages, args...)
	if err != nil {
		return modelrepo.Message{}, fmt.Errorf("OpenAI chat execution failed: %w", err)
	}

	return resp.Message, nil
}

var _ modelrepo.LLMPromptExecClient = (*OpenAIPromptClient)(nil)
