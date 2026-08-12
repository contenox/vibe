package ollama

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

type OllamaStreamClient struct {
	ollamaClient    *ollamaHTTPClient
	modelName       string
	backendURL      string
	maxOutputTokens int
	supportsThink   bool
	tracker         libtracker.ActivityTracker
}

func (c *OllamaStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "ollama", "model", c.modelName)

	// No audio encoding on this wire format; refuse instead of dropping silently.
	if err := modelrepo.RefuseAudioInput("ollama", c.modelName, messages); err != nil {
		reportErr(err)
		end()
		return nil, err
	}

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

	stream := true
	var think *ThinkValue
	if c.supportsThink {
		think = buildOllamaThink(config)
	}
	apiTools, err := buildOllamaTools(config)
	if err != nil {
		reportErr(err)
		end()
		return nil, err
	}
	req := &ChatRequest{
		Model:     c.modelName,
		Messages:  apiMessages,
		Stream:    &stream,
		Think:     think,
		Options:   buildOllamaOptions(config, c.maxOutputTokens),
		Tools:     apiTools,
		KeepAlive: keepAlive(),
	}
	if config.Shift != nil {
		req.Shift = config.Shift
	}
	if config.Truncate != nil {
		req.Truncate = config.Truncate
	}

	// Ollama delivers each tool call whole; each gets the next sequential index.
	ch := make(chan *modelrepo.StreamParcel)
	go func() {
		defer close(ch)
		defer end()

		send := func(p *modelrepo.StreamParcel) bool {
			select {
			case ch <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var (
			chunkCount    int
			totalLen      int
			toolCallIndex int
			terminal      *modelrepo.StreamTerminal
		)
		err := c.ollamaClient.Chat(ctx, req, func(resp ChatResponse) error {
			if resp.Message.Content != "" {
				chunkCount++
				totalLen += len(resp.Message.Content)
				if !send(&modelrepo.StreamParcel{Data: resp.Message.Content}) {
					return ctx.Err()
				}
			}
			if resp.Message.Thinking != "" {
				if !send(&modelrepo.StreamParcel{Thinking: resp.Message.Thinking}) {
					return ctx.Err()
				}
			}
			for _, tc := range resp.Message.ToolCalls {
				argsJSON, err := json.Marshal(tc.Function.Arguments)
				if err != nil {
					continue
				}
				delta := &modelrepo.ToolCallDelta{
					Index:        toolCallIndex,
					Type:         "function",
					Name:         tc.Function.Name,
					ArgsFragment: string(argsJSON),
				}
				toolCallIndex++
				if !send(&modelrepo.StreamParcel{ToolCall: delta}) {
					return ctx.Err()
				}
			}
			if resp.Done {
				terminal = &modelrepo.StreamTerminal{
					FinishReason: resp.DoneReason,
					Usage: &modelrepo.TokenUsage{
						PromptTokens:     resp.Metrics.PromptEvalCount,
						CompletionTokens: resp.Metrics.EvalCount,
						TotalTokens:      resp.Metrics.PromptEvalCount + resp.Metrics.EvalCount,
					},
				}
			}
			return nil
		})
		if err != nil {
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: fmt.Errorf("ollama stream request failed for model %s: %w", c.modelName, err)})
			return
		}
		if terminal == nil {
			// A truncated stream must not read as success.
			err := fmt.Errorf("ollama stream for model %s ended without a done response", c.modelName)
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}
		if !send(&modelrepo.StreamParcel{Terminal: terminal}) {
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
