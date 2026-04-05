package ollama

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/ollama/ollama/api"
)

type OllamaStreamClient struct {
	ollamaClient *ollamaHTTPClient
	modelName    string
	backendURL   string
	tracker      libtracker.ActivityTracker
}

func (c *OllamaStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "ollama", "model", c.modelName)

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

	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	stream := true
	think := buildOllamaThink(config)
	apiTools, err := buildOllamaTools(config)
	if err != nil {
		reportErr(err)
		end()
		return nil, err
	}
	req := &api.ChatRequest{
		Model:    c.modelName,
		Messages: apiMessages,
		Stream:   &stream,
		Think:    &think,
		Options:  buildOllamaOptions(config),
		Tools:    apiTools,
	}
	if config.Shift != nil {
		req.Shift = config.Shift
	}
	if config.Truncate != nil {
		req.Truncate = config.Truncate
	}

	ch := make(chan *modelrepo.StreamParcel)
	go func() {
		defer close(ch)
		defer end()

		var (
			chunkCount int
			totalLen   int
		)
		err := c.ollamaClient.Chat(ctx, req, func(resp api.ChatResponse) error {
			if resp.Message.Content != "" || resp.Message.Thinking != "" {
				if resp.Message.Content != "" {
					chunkCount++
					totalLen += len(resp.Message.Content)
				}
				select {
				case ch <- &modelrepo.StreamParcel{
					Data:     resp.Message.Content,
					Thinking: resp.Message.Thinking,
				}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		if err != nil {
			reportErr(err)
			select {
			case ch <- &modelrepo.StreamParcel{Error: fmt.Errorf("ollama stream request failed for model %s: %w", c.modelName, err)}:
			case <-ctx.Done():
			}
			return
		}

		reportChange("stream_completed", map[string]any{
			"chunk_count":  chunkCount,
			"total_length": totalLen,
		})
	}()

	return ch, nil
}

var _ modelrepo.LLMStreamClient = (*OllamaStreamClient)(nil)
