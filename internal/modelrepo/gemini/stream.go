package gemini

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

type GeminiStreamClient struct {
	geminiClient
}

func (c *GeminiStreamClient) Stream(ctx context.Context, prompt string, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	parcels := make(chan *modelrepo.StreamParcel)

	messages := []modelrepo.Message{
		{Role: "user", Content: prompt},
	}
	request := buildGeminiRequest(c.modelName, messages, nil, args)

	go func() {
		defer close(parcels)

		body, err := json.Marshal(request)
		if err != nil {
			parcels <- &modelrepo.StreamParcel{Error: fmt.Errorf("failed to marshal stream request: %w", err)}
			return
		}

		endpoint := fmt.Sprintf("/v1beta/models/%s:streamGenerateContent?alt=sse", c.modelName)
		fullURL := fmt.Sprintf("%s%s", c.baseURL, endpoint)

		tracker := c.tracker
		reportErr, reportChange, end := tracker.Start(
			ctx,
			"http_stream",
			"gemini",
			"model", c.modelName,
			"endpoint", endpoint,
			"base_url", c.baseURL,
		)
		defer end()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewBuffer(body))
		if err != nil {
			err = fmt.Errorf("failed to create stream request: %w", err)
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Api-Key", c.apiKey)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Connection", "keep-alive")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			err = fmt.Errorf("HTTP stream request failed for model %s: %w", c.modelName, err)
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}
		defer resp.Body.Close()

		// Log headers
		reportChange("gemini_stream_response", map[string]any{
			"status":  resp.StatusCode,
			"headers": resp.Header,
		})

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("gemini API returned non-200 status for stream: %d, body: %s", resp.StatusCode, string(b))
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}

		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			jsonData := strings.TrimPrefix(line, "data: ")
			if jsonData == "" || jsonData == "[DONE]" {
				continue
			}

			var chunk geminiGenerateContentResponse
			if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
				// ignore malformed frame; continue
				continue
			}

			if chunk.PromptFeedback.BlockReason != "" {
				err = fmt.Errorf("stream blocked by API for reason: %s", chunk.PromptFeedback.BlockReason)
				reportErr(err)
				parcels <- &modelrepo.StreamParcel{Error: err}
				return
			}
			if len(chunk.Candidates) > 0 && len(chunk.Candidates[0].Content.Parts) > 0 {
				text := chunk.Candidates[0].Content.Parts[0].Text
				if text != "" {
					select {
					case parcels <- &modelrepo.StreamParcel{Data: text}:
					case <-ctx.Done():
						return
					}
				}
			}
		}

		if err := sc.Err(); err != nil && err != io.EOF {
			err = fmt.Errorf("error reading from stream: %w", err)
			reportErr(err)
			select {
			case parcels <- &modelrepo.StreamParcel{Error: err}:
			case <-ctx.Done():
			}
		}
	}()

	return parcels, nil
}

var _ modelrepo.LLMStreamClient = (*GeminiStreamClient)(nil)
