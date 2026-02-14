package vllm

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
	"github.com/contenox/vibe/libtracker"
)

type VLLMStreamClient struct {
	vLLMClient
}

func NewVLLMStreamClient(ctx context.Context, baseURL, modelName string, contextLength int, httpClient *http.Client, apiKey string, tracker libtracker.ActivityTracker) (modelrepo.LLMStreamClient, error) {
	client := &VLLMStreamClient{
		vLLMClient: vLLMClient{
			baseURL:    baseURL,
			httpClient: httpClient,
			modelName:  modelName,
			apiKey:     apiKey,
			tracker:    tracker,
		},
	}

	client.maxTokens = min(contextLength, 2048)
	return client, nil
}

// Stream implements LLMStreamClient interface
func (c *VLLMStreamClient) Stream(ctx context.Context, prompt string, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	// Start tracking the operation
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "vllm", "model", c.modelName)
	// Note: We don't defer end() here because the stream is asynchronous

	// Convert prompt to message format
	messages := []modelrepo.Message{
		{Role: "user", Content: prompt},
	}

	config := &modelrepo.ChatConfig{}
	for _, arg := range args {
		arg.Apply(config)
	}

	request := chatRequest{
		Model:       c.modelName,
		Messages:    messages,
		Temperature: config.Temperature,
		MaxTokens:   config.MaxTokens,
		TopP:        config.TopP,
		Seed:        config.Seed,
		Stream:      true,
	}

	// Prepare the request
	url := c.baseURL + "/v1/chat/completions"
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
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

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
		err = fmt.Errorf("vLLM API returned non-200 status: %d - %s for model %s",
			resp.StatusCode, string(body), c.modelName)
		reportErr(err)
		end()
		return nil, err
	}

	// Process the stream in a separate goroutine
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

				var chunk chatStreamResponse
				if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
					// ignore malformed frame; continue
					continue
				}

				// Handle error chunks
				if chunk.Error != nil {
					err := fmt.Errorf("vLLM stream error: %s", *chunk.Error)
					reportErr(err)
					select {
					case streamCh <- &modelrepo.StreamParcel{
						Error: err,
					}:
					case <-ctx.Done():
					}
					return
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
			err := fmt.Errorf("stream scanning error: %w", err)
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

// chatStreamResponse represents a single chunk in the streaming response
type chatStreamResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		} `json:"delta"`
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices,omitempty"`
	Error *string `json:"error,omitempty"`
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

var _ modelrepo.LLMStreamClient = (*VLLMStreamClient)(nil)
