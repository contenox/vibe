package ollama

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/ollama/ollama/api"
)

type OllamaChatClient struct {
	ollamaClient *ollamaHTTPClient
	modelName    string
	backendURL   string
	tracker      libtracker.ActivityTracker
}

func (c *OllamaChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "ollama", "model", c.modelName)
	defer end()

	// Convert messages to Ollama API format (we preserve role, including "tool").
	// We must also map ToolCalls from assistant messages so Ollama knows what tools
	// were already called — without this the LLM has no context of its prior tool calls.
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
			ToolCalls: apiToolCalls,
		})
	}

	// Build configuration from arguments
	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	llamaOptions := buildOllamaOptions(config)
	think := buildOllamaThink(config)
	stream := false

	apiTools, err := buildOllamaTools(config)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	req := &api.ChatRequest{
		Model:    c.modelName,
		Messages: apiMessages,
		Stream:   &stream,
		Think:    &think,
		Options:  llamaOptions,
		Tools:    apiTools,
	}
	if config.Shift != nil {
		req.Shift = config.Shift
	}
	if config.Truncate != nil {
		req.Truncate = config.Truncate
	}

	var finalResponse api.ChatResponse

	// Handle the API call
	err = c.ollamaClient.Chat(ctx, req, func(res api.ChatResponse) error {
		// We keep only the final frame; Ollama includes the full message there
		if res.Done {
			finalResponse = res
		}
		return nil
	})

	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, fmt.Errorf("ollama API chat request failed for model %s: %w", c.modelName, err)
	}

	// Check if we received any response
	if finalResponse.Message.Role == "" {
		err := fmt.Errorf("no response received from ollama for model %s", c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	// Handle completion reasons
	switch finalResponse.DoneReason {
	case "error":
		err := fmt.Errorf("ollama generation error for model %s: %s", c.modelName, finalResponse.Message.Content)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	case "length":
		err := fmt.Errorf("token limit reached for model %s (partial response: %q)", c.modelName, finalResponse.Message.Content)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	case "stop":
		// Allow empty content if there are tool calls — the model is signalling it wants to call tools.
		// Also allow empty content with no tool calls — some models (e.g. qwen2.5) emit this as a
		// "done" signal after completing a tool-use loop. Callers can detect this from content and tool_calls being empty.
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", finalResponse.DoneReason, c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	// Base assistant message
	message := modelrepo.Message{
		Role:     finalResponse.Message.Role,
		Content:  finalResponse.Message.Content,
		Thinking: finalResponse.Message.Thinking,
	}

	// Convert Ollama tool calls → modelrepo.ToolCall
	// Ollama ToolCall:
	//   type ToolCall struct {
	//       Function ToolCallFunction `json:"function"`
	//   }
	//   type ToolCallFunction struct {
	//       Index     int                       `json:"index"`
	//       Name      string                    `json:"name"`
	//       Arguments ToolCallFunctionArguments `json:"arguments"`
	//   }
	//   type ToolCallFunctionArguments map[string]any
	//   func (t *ToolCallFunctionArguments) String() string { ... }
	var toolCalls []modelrepo.ToolCall
	for i, tc := range finalResponse.Message.ToolCalls {
		argsJSON := tc.Function.Arguments.String()

		toolCalls = append(toolCalls, modelrepo.ToolCall{
			ID:   fmt.Sprintf("ollama-tool-%d", i),
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
	}

	reportChange("chat_completed", map[string]any{
		"content_length":   len(message.Content),
		"tool_calls_count": len(toolCalls),
		"done_reason":      finalResponse.DoneReason,
	})
	return result, nil
}

var _ modelrepo.LLMChatClient = (*OllamaChatClient)(nil)
