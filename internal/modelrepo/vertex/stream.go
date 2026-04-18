package vertex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
)

type vertexStreamClient struct {
	vertexClient
}

// Stream implements modelrepo.LLMStreamClient.
func (c *vertexStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	parcels := make(chan *modelrepo.StreamParcel)

	request, err := buildVertexRequest(messages, args)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(parcels)

		body, err := json.Marshal(request)
		if err != nil {
			parcels <- &modelrepo.StreamParcel{Error: fmt.Errorf("failed to marshal stream request: %w", err)}
			return
		}

		endpoint := c.endpoint("streamGenerateContent") + "?alt=sse"

		reportErr, reportChange, end := c.tracker.Start(
			ctx,
			"http_stream",
			"vertex",
			"model", c.modelName,
			"publisher", c.publisher,
			"endpoint", endpoint,
		)
		defer end()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(body))
		if err != nil {
			err = fmt.Errorf("failed to create stream request: %w", err)
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Connection", "keep-alive")

		tokenFn := c.tokenFn
		if tokenFn == nil {
			tokenFn = func(ctx context.Context) (string, error) {
				return BearerTokenWithCreds(ctx, c.credJSON)
			}
		}
		token, err := tokenFn(ctx)
		if err != nil {
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			err = fmt.Errorf("HTTP stream request failed for model %s: %w", c.modelName, err)
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}
		defer resp.Body.Close()

		reportChange("vertex_stream_response", map[string]any{
			"status":  resp.StatusCode,
			"headers": resp.Header,
		})

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("vertex API returned non-200 status for stream: %d, body: %s", resp.StatusCode, string(b))
			reportErr(err)
			parcels <- &modelrepo.StreamParcel{Error: err}
			return
		}

		var (
			chunkCount   int
			totalContent strings.Builder
		)

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

			var chunk vertexResponse
			if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
				continue
			}

			if chunk.PromptFeedback.BlockReason != "" {
				err = fmt.Errorf("stream blocked by Vertex AI for reason: %s", chunk.PromptFeedback.BlockReason)
				reportErr(err)
				parcels <- &modelrepo.StreamParcel{Error: err}
				return
			}

			if len(chunk.Candidates) > 0 && len(chunk.Candidates[0].Content.Parts) > 0 {
				var outText, thinkingText string
				for _, part := range chunk.Candidates[0].Content.Parts {
					switch {
					case part.Thought && part.Text != "":
						thinkingText += part.Text
					case part.Text != "":
						outText += part.Text
					}
				}
				if outText != "" || thinkingText != "" {
					chunkCount++
					totalContent.WriteString(outText)
					select {
					case parcels <- &modelrepo.StreamParcel{Data: outText, Thinking: thinkingText}:
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
			return
		}

		reportChange("stream_completed", map[string]any{
			"chunk_count":     chunkCount,
			"total_length":    totalContent.Len(),
			"content_preview": truncateString(totalContent.String(), 100),
		})
	}()

	return parcels, nil
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var _ modelrepo.LLMStreamClient = (*vertexStreamClient)(nil)
