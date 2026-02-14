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

	"github.com/contenox/vibe/internal/modelrepo"
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
			Role      string `json:"role,omitempty"`
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
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

func (c *OpenAIStreamClient) Stream(ctx context.Context, prompt string, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "openai", "model", c.modelName)
	// Note: We don't defer end() here because the stream is asynchronous

	messages := []modelrepo.Message{{Role: "user", Content: prompt}}

	// buildOpenAIRequest now returns (request, nameMap); we only need the request here.
	request, _ := buildOpenAIRequest(c.modelName, messages, args)
	request.Stream = true

	url := c.baseURL + "/chat/completions"
	reqBody, err := json.Marshal(request)
	if err != nil {
		end()
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		end()
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	streamCh := make(chan *modelrepo.StreamParcel)

	resp, err := c.httpClient.Do(req)
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
					if delta.Content != "" {
						chunkCount++
						totalContent.WriteString(delta.Content)
						select {
						case streamCh <- &modelrepo.StreamParcel{Data: delta.Content}:
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

var _ modelrepo.LLMStreamClient = (*OpenAIStreamClient)(nil)
