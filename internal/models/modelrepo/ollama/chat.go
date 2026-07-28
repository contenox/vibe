package ollama

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/google/uuid"
	"github.com/ollama/ollama/api"
)

type OllamaChatClient struct {
	ollamaClient    *ollamaHTTPClient
	modelName       string
	backendURL      string
	maxOutputTokens int
	supportsThink   bool
	tracker         libtracker.ActivityTracker
}

// toOllamaImages maps image attachments to the Ollama SDK's image list. Ollama
// carries raw image bytes (base64-encoded on the wire) and sniffs the format
// itself, so MimeType is not sent.
func toOllamaImages(images []modelrepo.ImagePart) []api.ImageData {
	if len(images) == 0 {
		return nil
	}
	out := make([]api.ImageData, 0, len(images))
	for _, img := range images {
		out = append(out, api.ImageData(img.Data))
	}
	return out
}

func (c *OllamaChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "ollama", "model", c.modelName)
	defer end()

	// Prior ToolCalls must be mapped too, or Ollama has no record of what
	// tools were already called.
	apiMessages := make([]api.Message, 0, len(messages))
	for _, msg := range messages {
		var apiToolCalls []api.ToolCall
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				argsStr := tc.Function.Arguments
				if argsStr == "" {
					argsStr = "{}"
				}
				var tcArgs api.ToolCallFunctionArguments
				_ = json.Unmarshal([]byte(argsStr), &tcArgs)
				apiToolCalls = append(apiToolCalls, api.ToolCall{
					Function: api.ToolCallFunction{
						Name:      tc.Function.Name,
						Arguments: tcArgs,
					},
				})
			}
		}
		apiMessages = append(apiMessages, api.Message{
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
	var think *api.ThinkValue
	if c.supportsThink {
		think = buildOllamaThink(config)
	}
	stream := false

	apiTools, err := buildOllamaTools(config)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	req := &api.ChatRequest{
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

	var finalResponse api.ChatResponse

	// Only the final frame is kept; Ollama includes the full message there.
	err = c.ollamaClient.Chat(ctx, req, func(res api.ChatResponse) error {
		if res.Done {
			finalResponse = res
		}
		return nil
	})

	if err != nil {
		reportErr(err)
		wrapped := fmt.Errorf("ollama API chat request failed for model %s: %w", c.modelName, err)
		// Ollama reports context overflow only as an error string; classify it
		// so callers get the typed sentinel.
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
		// Truncated but successful, same contract as the streaming assembler:
		// partial content is real and the finish reason surfaces the truncation.
	case "stop":
		// Empty content is allowed with tool calls (model wants to call tools)
		// or without (some models, e.g. qwen2.5, use this as an end-of-tool-loop signal).
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
		// Arguments is a map; String() renders it as the JSON string modelrepo.ToolCall expects.
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
		// Ollama reports no cache dimension: prompt_eval_count already includes
		// tokens reused from the per-slot KV cache, so PromptTokens is the
		// total and the cache fields stay zero.
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
