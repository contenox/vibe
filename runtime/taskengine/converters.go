package taskengine

import "time"

// NormalizeFinalChainOutput upgrades DataTypeAny to a concrete DataType and coerces the value.
// ExecEnv calls this before returning so API layers see stable output types.
func NormalizeFinalChainOutput(value any, dt DataType) (any, DataType, error) {
	return NormalizeDataType(value, dt)
}

// ConvertChatHistoryToOpenAI converts the internal ChatHistory format to an OpenAI-compatible response.
// This is useful for adapting the task engine's output to systems expecting an OpenAI API format.
func ConvertChatHistoryToOpenAI(id string, chatHistory ChatHistory) OpenAIChatResponse {
	resp := OpenAIChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   chatHistory.Model,
		Usage: OpenAITokenUsage{
			PromptTokens:     chatHistory.InputTokens,
			CompletionTokens: chatHistory.OutputTokens,
			TotalTokens:      chatHistory.InputTokens + chatHistory.OutputTokens,
		},
		Choices: []OpenAIChatResponseChoice{},
	}
	resp.Model = chatHistory.Model
	// The last message in the history is assumed to be the assistant's completion.
	if len(chatHistory.Messages) > 0 {
		lastMessage := chatHistory.Messages[len(chatHistory.Messages)-1]
		choice := OpenAIChatResponseChoice{
			Index: 0,
			Message: OpenAIChatResponseMessage{
				Role: lastMessage.Role,
			},
		}

		if len(lastMessage.CallTools) > 0 {
			choice.FinishReason = "tool_calls"
			choice.Message.ToolCalls = lastMessage.CallTools
			// Content remains nil for tool calls
		} else {
			choice.FinishReason = "stop"
			// Pointer to the content string
			content := lastMessage.Content
			choice.Message.Content = &content
			choice.Message.Thinking = lastMessage.Thinking
		}

		resp.Choices = append(resp.Choices, choice)
	}

	return resp
}

// ConvertOpenAIToChatHistory converts an OpenAI-compatible chat request into the internal
// ChatHistory and LLMExecutionConfig formats used by the task engine.
func ConvertOpenAIToChatHistory(request OpenAIChatRequest) (ChatHistory, int, []Tool, LLMExecutionConfig) {
	chatHistory := ChatHistory{
		Model:    request.Model,
		Messages: make([]Message, 0, len(request.Messages)),
	}

	for _, reqMsg := range request.Messages {
		chatHistory.Messages = append(chatHistory.Messages, Message{
			Role:       reqMsg.Role,
			Content:    reqMsg.Content,
			Thinking:   reqMsg.Thinking,
			CallTools:  reqMsg.ToolCalls,
			ToolCallID: reqMsg.ToolCallID,
			Timestamp:  time.Now().UTC(),
		})
	}

	config := LLMExecutionConfig{
		Model:       request.Model,
		Temperature: float32(request.Temperature),
	}

	return chatHistory, request.MaxTokens, request.Tools, config
}

func ConvertChatHistoryToOpenAIRequest(
	chatHistory ChatHistory,
) (OpenAIChatRequest, int, int) {
	model := chatHistory.Model

	// Prepare messages
	messages := make([]OpenAIChatRequestMessage, 0, len(chatHistory.Messages))
	for _, msg := range chatHistory.Messages {
		messages = append(messages, OpenAIChatRequestMessage{
			Role:       msg.Role,
			Content:    msg.Content,
			Thinking:   msg.Thinking,
			ToolCalls:  msg.CallTools,
			ToolCallID: msg.ToolCallID,
		})
	}

	temperature := 0.0

	return OpenAIChatRequest{
		Model:            model,
		Messages:         messages,
		Temperature:      temperature,
		TopP:             0.0,
		PresencePenalty:  0.0,
		FrequencyPenalty: 0.0,
		N:                0,
		Stream:           false,
	}, chatHistory.InputTokens, chatHistory.OutputTokens
}
