package openaichatservice

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/vibe/execservice"
	"github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
	"github.com/google/uuid"
)

type Service interface {
	OpenAIChatCompletions(ctx context.Context, taskChainID string, req taskengine.OpenAIChatRequest) (*taskengine.OpenAIChatResponse, []taskengine.CapturedStateUnit, error)
	OpenAIChatCompletionsStream(ctx context.Context, taskChainID string, req taskengine.OpenAIChatRequest, speed time.Duration) (<-chan OpenAIChatStreamChunk, error)
}

type service struct {
	dbInstance   libdbexec.DBManager
	chainService taskchainservice.Service
	env          execservice.TasksEnvService
}

func New(
	env execservice.TasksEnvService,
	chainService taskchainservice.Service,
) Service {
	return &service{
		chainService: chainService,
		env:          env,
	}
}

func (s *service) OpenAIChatCompletions(ctx context.Context, taskChainID string, req taskengine.OpenAIChatRequest) (*taskengine.OpenAIChatResponse, []taskengine.CapturedStateUnit, error) {
	chain, err := s.chainService.Get(ctx, taskChainID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load task chain '%s': %w", taskChainID, err)
	}

	// validator := workflowvalidator.New()
	// if err := validator.ValidateWorkflow(chain, workflowvalidator.OpenAIChatServiceProfile); err != nil {
	// 	return nil, nil, fmt.Errorf("workflow validation failed: %w", err)
	// }

	result, dt, stackTrace, err := s.env.Execute(ctx, chain, req, taskengine.DataTypeOpenAIChat)
	if err != nil {
		return nil, stackTrace, fmt.Errorf("chain execution failed: %w", err)
	}

	if result == nil {
		return nil, stackTrace, fmt.Errorf("empty result from chain execution")
	}

	switch dt {
	case taskengine.DataTypeChatHistory, taskengine.DataTypeOpenAIChatResponse:
		if res, ok := result.(taskengine.ChatHistory); ok {
			id := fmt.Sprintf("%d-%s", time.Now().Unix(), uuid.NewString()[:4])
			result = taskengine.ConvertChatHistoryToOpenAI(id, res)
		}

		res, ok := result.(taskengine.OpenAIChatResponse)
		if !ok {
			return nil, stackTrace, fmt.Errorf("invalid result type from chain: %T", result)
		}

		return &res, stackTrace, nil
	default:
		return nil, stackTrace, fmt.Errorf("invalid result type from chain: dt: %s-%T", dt.String(), result)
	}

}

// OpenAIChatCompletionsStream gets the full response and streams it back word-by-word.
func (s *service) OpenAIChatCompletionsStream(ctx context.Context, taskChainID string, req taskengine.OpenAIChatRequest, speed time.Duration) (<-chan OpenAIChatStreamChunk, error) {
	fullResponse, _, err := s.OpenAIChatCompletions(ctx, taskChainID, req)
	if err != nil {
		return nil, err
	}

	if len(fullResponse.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned from the model")
	}

	ch := make(chan OpenAIChatStreamChunk)

	go func() {
		defer close(ch)

		choice := fullResponse.Choices[0]
		finishReason := "stop"

		// Handle tool call "streaming"
		if len(choice.Message.ToolCalls) > 0 {
			finishReason = "tool_calls"
			toolCallChunks := make([]OpenAIStreamToolCall, len(choice.Message.ToolCalls))
			for i, tc := range choice.Message.ToolCalls {
				toolCallChunks[i] = OpenAIStreamToolCall{
					Index: i,
					ID:    tc.ID,
					Type:  tc.Type,
					Function: &OpenAIStreamFunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}

			chunk := OpenAIChatStreamChunk{
				ID:      fullResponse.ID,
				Object:  "chat.completion.chunk",
				Created: fullResponse.Created,
				Model:   fullResponse.Model,
				Choices: []OpenAIStreamChoice{
					{
						Index: 0,
						Delta: OpenAIStreamDelta{
							ToolCalls: toolCallChunks,
						},
					},
				},
			}
			ch <- chunk
		} else if choice.Message.Content != nil {
			// Handle content streaming
			content := *choice.Message.Content
			words := strings.Fields(content)

			// Stream word by word
			for i, word := range words {
				chunk := OpenAIChatStreamChunk{
					ID:      fullResponse.ID,
					Object:  "chat.completion.chunk",
					Created: fullResponse.Created,
					Model:   fullResponse.Model,
					Choices: []OpenAIStreamChoice{
						{
							Index: 0,
							Delta: OpenAIStreamDelta{
								Content: " " + word,
							},
						},
					},
				}
				// Remove leading space for the very first word
				if i == 0 {
					chunk.Choices[0].Delta.Content = word
				}
				ch <- chunk
				time.Sleep(speed)
			}
		}

		// Send the final chunk with the appropriate finish reason
		finalChunk := OpenAIChatStreamChunk{
			ID:      fullResponse.ID,
			Object:  "chat.completion.chunk",
			Created: fullResponse.Created,
			Model:   fullResponse.Model,
			Choices: []OpenAIStreamChoice{
				{
					Index:        0,
					Delta:        OpenAIStreamDelta{},
					FinishReason: &finishReason,
				},
			},
		}
		ch <- finalChunk
	}()

	return ch, nil
}

// OpenAIChatStreamChunk represents a single chunk of data in an SSE stream
// for an OpenAI-compatible chat completion.
type OpenAIChatStreamChunk struct {
	ID      string               `json:"id" example:"chatcmpl-123"`
	Object  string               `json:"object" example:"chat.completion.chunk"`
	Created int64                `json:"created" example:"1694268190"`
	Model   string               `json:"model" example:"mistral:instruct"`
	Choices []OpenAIStreamChoice `json:"choices" openapi_include_type:"chatservice.OpenAIStreamChoice"`
}

// OpenAIStreamChoice represents a choice within a stream chunk. It contains the
// delta, which is the actual content being streamed.
type OpenAIStreamChoice struct {
	Index        int               `json:"index" example:"0"`
	Delta        OpenAIStreamDelta `json:"delta" openapi_include_type:"chatservice.OpenAIStreamDelta"`
	FinishReason *string           `json:"finish_reason,omitempty" example:"stop"`
}

// OpenAIStreamToolCall represents a tool call within a stream chunk.
type OpenAIStreamToolCall struct {
	Index    int                       `json:"index"`
	ID       string                    `json:"id,omitempty"`
	Type     string                    `json:"type,omitempty"`
	Function *OpenAIStreamFunctionCall `json:"function,omitempty"`
}

// OpenAIStreamFunctionCall represents function details within a tool call stream chunk.
type OpenAIStreamFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// OpenAIStreamDelta contains the incremental content update (the "delta") for a
// streaming chat response.
type OpenAIStreamDelta struct {
	// The role of the author of this message.
	Role string `json:"role,omitempty" example:"assistant"`
	// The contents of the chunk.
	Content   string                 `json:"content,omitempty" example:" world"`
	ToolCalls []OpenAIStreamToolCall `json:"tool_calls,omitempty"`
}
