package ollama

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/google/uuid"
)

type OllamaChatClient struct {
	ollamaClient    *ollamaHTTPClient
	modelName       string
	backendURL      string
	maxOutputTokens int
	supportsThink   bool
	tracker         libtracker.ActivityTracker
}

func toOllamaImages(images []modelrepo.ImagePart) []ImageData {
	if len(images) == 0 {
		return nil
	}
	out := make([]ImageData, 0, len(images))
	for _, img := range images {
		out = append(out, ImageData(img.Data))
	}
	return out
}

func (c *OllamaChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "ollama", "model", c.modelName)
	defer end()

	// No audio encoding on this wire format; refuse instead of dropping silently.
	if err := modelrepo.RefuseAudioInput("ollama", c.modelName, messages); err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	// Prior ToolCalls must be mapped too, or Ollama has no record of tools already called.
	apiMessages := make([]Message, 0, len(messages))
	for _, msg := range messages {
		var apiToolCalls []ToolCall
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				argsStr := tc.Function.Arguments
				if argsStr == "" {
					argsStr = "{}"
				}
				var tcArgs ToolCallFunctionArguments
				_ = json.Unmarshal([]byte(argsStr), &tcArgs)
				apiToolCalls = append(apiToolCalls, ToolCall{
					Function: ToolCallFunction{
						Name:      tc.Function.Name,
						Arguments: tcArgs,
					},
				})
			}
		}
		apiMessages = append(apiMessages, Message{
			Role:      msg.Role,
			Content:   msg.Content,
			Images:    toOllamaImages(msg.Images),
			ToolCalls: apiToolCalls,
		})
	}

	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	llamaOptions := buildOllamaOptions(config, c.maxOutputTokens)
	var think *ThinkValue
	if c.supportsThink {
		think = buildOllamaThink(config)
	}
	stream := false

	apiTools, err := buildOllamaTools(config)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	req := &ChatRequest{
		Model:     c.modelName,
		Messages:  apiMessages,
		Stream:    &stream,
		Think:     think,
		Options:   llamaOptions,
		Tools:     apiTools,
		KeepAlive: keepAlive(),
	}
	if config.Shift != nil {
		req.Shift = config.Shift
	}
	if config.Truncate != nil {
		req.Truncate = config.Truncate
	}

	var finalResponse ChatResponse

	// Only the final frame is kept; Ollama includes the full message there.
	err = c.ollamaClient.Chat(ctx, req, func(res ChatResponse) error {
		if res.Done {
			finalResponse = res
		}
		return nil
	})

	if err != nil {
		reportErr(err)
		wrapped := fmt.Errorf("ollama API chat request failed for model %s: %w", c.modelName, err)
		// Ollama reports context overflow only as an error string; classify it into the typed sentinel.
		return modelrepo.ChatResult{}, modelrepo.ClassifyProviderError(wrapped, 0, "", err.Error())
	}

	if finalResponse.Message.Role == "" {
		err := fmt.Errorf("no response received from ollama for model %s", c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	switch finalResponse.DoneReason {
	case "error":
		err := fmt.Errorf("ollama generation error for model %s: %s", c.modelName, finalResponse.Message.Content)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	case "length":
		// Truncated but successful; partial content is real.
	case "stop":
		// Empty content is allowed with or without tool calls.
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", finalResponse.DoneReason, c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	message := modelrepo.Message{
		Role:     finalResponse.Message.Role,
		Content:  finalResponse.Message.Content,
		Thinking: finalResponse.Message.Thinking,
	}

	var toolCalls []modelrepo.ToolCall
	for _, tc := range finalResponse.Message.ToolCalls {
		// Arguments is an ordered map; String() renders it as the JSON string modelrepo.ToolCall expects.
		argsJSON := tc.Function.Arguments.String()

		toolCalls = append(toolCalls, modelrepo.ToolCall{
			ID:   uuid.NewString(),
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{
				Name:      tc.Function.Name,
				Arguments: argsJSON,
			},
		})
	}

	result := modelrepo.ChatResult{
		Message:   message,
		ToolCalls: toolCalls,
		// Ollama reports no cache dimension: prompt_eval_count is already the total.
		Usage: &modelrepo.TokenUsage{
			PromptTokens:     finalResponse.Metrics.PromptEvalCount,
			CompletionTokens: finalResponse.Metrics.EvalCount,
			TotalTokens:      finalResponse.Metrics.PromptEvalCount + finalResponse.Metrics.EvalCount,
		},
		FinishReason: finalResponse.DoneReason,
	}

	reportChange("chat_completed", map[string]any{
		"content_length":   len(message.Content),
		"tool_calls_count": len(toolCalls),
		"done_reason":      finalResponse.DoneReason,
	})
	return result, nil
}

var _ modelrepo.LLMChatClient = (*OllamaChatClient)(nil)
