package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/beam/internal/models/modelrepo"
)

type OpenAIStreamClient struct {
	openAIClient
}

type openAIChatStreamResponseChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			ToolCalls        []struct {
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (c *OpenAIStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "openai", "model", c.modelName)
	// Note: We don't defer end() here because the stream is asynchronous

	streamCh := make(chan *modelrepo.StreamParcel)
	usesResponses := openAIUsesResponsesEndpoint(c.modelName)
	endpoint := "/chat/completions"
	var requestBody []byte
	var responseNameMap map[string]string
	var err error

	if usesResponses {
		var req openAIResponsesRequest
		req, responseNameMap = buildOpenAIResponsesRequestWithCapabilities(c.modelName, messages, args, c.supportsThink)
		c.clampResponsesMaxOutputTokens(&req)
		req.Stream = true
		requestBody, err = json.Marshal(req)
		endpoint = "/responses"
	} else {
		var req openAIChatRequest
		req, _ = buildOpenAIRequestWithCapabilities(c.modelName, messages, args, c.supportsThink)
		req.Stream = true
		c.clampChatMaxOutputTokens(&req)
		requestBody, err = json.Marshal(req)
	}
	if err != nil {
		end()
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+endpoint, bytes.NewBuffer(requestBody))
	if err != nil {
		end()
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		err = fmt.Errorf("HTTP request failed for model %s: %w", c.modelName, err)
		reportErr(err)
		end()
		return nil, err
	}

	// Check response status
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("OpenAI API returned non-200 status: %d - %s for model %s",
			resp.StatusCode, string(body), c.modelName)
		reportErr(err)
		end()
		return nil, err
	}

	go func() {
		defer close(streamCh)
		defer resp.Body.Close()
		defer end() // End tracking when the stream completes

		if usesResponses {
			streamResponsesSSE(ctx, resp.Body, responseNameMap, streamCh, reportErr, reportChange)
			return
		}

		// Create a scanner to read the response line by line
		scanner := bufio.NewScanner(resp.Body)
		var chunkCount int
		var totalContent strings.Builder

		for scanner.Scan() {
			line := scanner.Text()

			// Skip empty lines
			if line == "" {
				continue
			}

			// SSE format starts with "data: "
			if strings.HasPrefix(line, "data: ") {
				jsonData := strings.TrimPrefix(line, "data: ")

				// Skip [DONE] messages
				if jsonData == "[DONE]" {
					continue
				}

				var chunk openAIChatStreamResponseChunk
				if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
					select {
					case streamCh <- &modelrepo.StreamParcel{
						Error: fmt.Errorf("failed to decode SSE data: %w, raw: %s", err, jsonData),
					}:
					case <-ctx.Done():
						return
					}
					continue
				}

				// Process the chunk
				if len(chunk.Choices) > 0 {
					delta := chunk.Choices[0].Delta
					if delta.Content != "" || delta.ReasoningContent != "" {
						if delta.Content != "" {
							chunkCount++
							totalContent.WriteString(delta.Content)
						}
						select {
						case streamCh <- &modelrepo.StreamParcel{
							Data:     delta.Content,
							Thinking: delta.ReasoningContent,
						}:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			err = fmt.Errorf("stream scanning error: %w", err)
			reportErr(err)
			select {
			case streamCh <- &modelrepo.StreamParcel{
				Error: err,
			}:
			case <-ctx.Done():
				return
			}
		}

		reportChange("stream_completed", map[string]any{
			"chunk_count":     chunkCount,
			"total_length":    totalContent.Len(),
			"content_preview": truncateString(totalContent.String(), 100),
		})
	}()

	return streamCh, nil
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// responsesSSEEvent covers the subset of Responses API SSE event types we handle.
type responsesSSEEvent struct {
	Type string `json:"type"`
	// response.output_text.delta
	Delta string `json:"delta"`
	// response.function_call_arguments.delta
	CallID string `json:"call_id"`
	// response.output_item.done — the finished item
	Item *openAIResponseOutputItem `json:"item"`
	// response.completed — the full response (reasoning summary lives here)
	Response *openAIResponse `json:"response"`
	// error — code/message are top-level per the Responses API spec, but some
	// gateways nest them under an "error" object instead.
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errorText resolves the error detail from either encoding, falling back to
// the raw payload so a stream failure never surfaces as an empty error.
func (ev responsesSSEEvent) errorText(rawPayload string) string {
	code, msg := ev.Code, ev.Message
	if ev.Error != nil {
		if code == "" {
			code = ev.Error.Code
		}
		if msg == "" {
			msg = ev.Error.Message
		}
	}
	if ev.Response != nil && ev.Response.Error != nil {
		if code == "" {
			code = ev.Response.Error.Code
		}
		if msg == "" {
			msg = ev.Response.Error.Message
		}
	}
	if code == "" && msg == "" {
		return truncateString(rawPayload, 512)
	}
	return code + ": " + msg
}

// streamResponsesSSE reads a Responses API SSE stream and forwards text parcels
// to out. Tool call arguments are accumulated but not streamed (consistent with
// the Chat Completions stream path). Reasoning summaries are emitted as thinking
// parcels from the response.completed event.
func streamResponsesSSE(
	ctx context.Context,
	body io.ReadCloser,
	nameMap map[string]string,
	out chan<- *modelrepo.StreamParcel,
	reportErr func(error),
	reportChange func(string, any),
) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var chunkCount int

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var ev responsesSSEEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta == "" {
				continue
			}
			chunkCount++
			select {
			case out <- &modelrepo.StreamParcel{Data: ev.Delta}:
			case <-ctx.Done():
				return
			}

		case "response.function_call_arguments.delta":
			// Tool call args are accumulated server-side; the final arguments
			// appear in response.output_item.done. Nothing to emit here.

		case "response.completed":
			// Emit reasoning summary (if any) as a thinking parcel.
			if ev.Response != nil && ev.Response.Reasoning.Summary != "" {
				select {
				case out <- &modelrepo.StreamParcel{Thinking: ev.Response.Reasoning.Summary}:
				case <-ctx.Done():
					return
				}
			}

		case "error":
			err := fmt.Errorf("responses stream error %s", ev.errorText(payload))
			reportErr(err)
			select {
			case out <- &modelrepo.StreamParcel{Error: err}:
			case <-ctx.Done():
			}
			return

		case "response.failed", "response.incomplete":
			// Terminal non-success events: without this the stream would end
			// silently and the caller would see an empty completion.
			err := fmt.Errorf("responses stream %s %s", ev.Type, ev.errorText(payload))
			reportErr(err)
			select {
			case out <- &modelrepo.StreamParcel{Error: err}:
			case <-ctx.Done():
			}
			return
		}
	}

	if err := sc.Err(); err != nil && err != io.EOF {
		err = fmt.Errorf("responses: stream read: %w", err)
		reportErr(err)
		select {
		case out <- &modelrepo.StreamParcel{Error: err}:
		case <-ctx.Done():
		}
		return
	}

	reportChange("stream_completed", map[string]any{
		"path":        "responses",
		"chunk_count": chunkCount,
	})
}

var _ modelrepo.LLMStreamClient = (*OpenAIStreamClient)(nil)
