package ollama

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
	"github.com/ollama/ollama/api"
)

type OllamaChatClient struct {
	ollamaClient *api.Client
	modelName    string
	backendURL   string
	tracker      libtracker.ActivityTracker
}

func (c *OllamaChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "ollama", "model", c.modelName)
	defer end()

	// Convert messages to Ollama API format (we preserve role, including "tool")
	apiMessages := make([]api.Message, 0, len(messages))
	for _, msg := range messages {
		apiMessages = append(apiMessages, api.Message{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	// Build configuration from arguments
	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	// Prepare Ollama options
	llamaOptions := make(map[string]any)

	if config.Temperature != nil {
		llamaOptions["temperature"] = *config.Temperature
	}

	if config.MaxTokens != nil {
		llamaOptions["num_predict"] = *config.MaxTokens
	}

	if config.TopP != nil {
		llamaOptions["top_p"] = *config.TopP
	}

	if config.Seed != nil {
		llamaOptions["seed"] = *config.Seed
	}

	think := api.ThinkValue{Value: false}
	stream := false

	// Convert modelrepo tools → Ollama tools using ToolFunctionParameters
	var apiTools api.Tools
	if len(config.Tools) > 0 {
		apiTools = make(api.Tools, 0, len(config.Tools))
		for _, tool := range config.Tools {
			// must be a function tool with a name
			if tool.Type == "" || tool.Function == nil || tool.Function.Name == "" {
				continue
			}

			var params api.ToolFunctionParameters
			if tool.Function.Parameters != nil {
				raw, err := json.Marshal(tool.Function.Parameters)
				if err != nil {
					reportErr(err)
					return modelrepo.ChatResult{}, fmt.Errorf(
						"failed to marshal tool parameters for %s: %w",
						tool.Function.Name, err,
					)
				}
				if err := json.Unmarshal(raw, &params); err != nil {
					reportErr(err)
					return modelrepo.ChatResult{}, fmt.Errorf(
						"failed to unmarshal tool parameters into ollama ToolFunctionParameters for %s: %w",
						tool.Function.Name, err,
					)
				}
			}

			apiTools = append(apiTools, api.Tool{
				Type: tool.Type,
				Function: api.ToolFunction{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  params,
				},
			})
		}
	}

	req := &api.ChatRequest{
		Model:    c.modelName,
		Messages: apiMessages,
		Stream:   &stream,
		Think:    &think,
		Options:  llamaOptions,
		Tools:    apiTools,
	}

	var finalResponse api.ChatResponse

	// Handle the API call
	err := c.ollamaClient.Chat(ctx, req, func(res api.ChatResponse) error {
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
		if finalResponse.Message.Content == "" && len(finalResponse.Message.ToolCalls) == 0 {
			err := fmt.Errorf("empty content from model %s despite normal completion", c.modelName)
			reportErr(err)
			return modelrepo.ChatResult{}, err
		}
	default:
		err := fmt.Errorf("unexpected completion reason %q for model %s", finalResponse.DoneReason, c.modelName)
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}

	// Base assistant message
	message := modelrepo.Message{
		Role:    finalResponse.Message.Role,
		Content: finalResponse.Message.Content,
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
