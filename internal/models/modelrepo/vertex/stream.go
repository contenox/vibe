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

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/google/uuid"
)

type vertexStreamClient struct {
	vertexClient
}

// Stream implements modelrepo.LLMStreamClient against the Gemini
// streamGenerateContent wire format (vertex-google).
func (c *vertexStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	parcels := make(chan *modelrepo.StreamParcel)

	request, err := buildVertexRequest(c.modelName, messages, args, c.canThink)
	if err != nil {
		return nil, err
	}
	if request.GenerationConfig != nil {
		request.GenerationConfig.MaxOutputTokens = modelrepo.ClampMaxOutputTokensPtr(request.GenerationConfig.MaxOutputTokens, c.maxOutputTokens)
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
		if project := extractProjectFromVertexURL(c.baseURL); project != "" {
			req.Header.Set("x-goog-user-project", project)
		}

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
			chunkCount    int
			totalContent  strings.Builder
			toolCalls     []modelrepo.ToolCall
			lastSignature string
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
					case part.FunctionCall != nil:
						argsJSON, err := json.Marshal(part.FunctionCall.Args)
						if err != nil {
							continue
						}
						tc := modelrepo.ToolCall{
							ID:   uuid.NewString(),
							Type: "function",
							Function: struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							}{
								Name:      part.FunctionCall.Name,
								Arguments: string(argsJSON),
							},
						}
						sig := part.ThoughtSignature
						if sig == "" {
							sig = part.FunctionCall.ThoughtSignature
						}
						if sig == "" {
							sig = lastSignature
						}
						if sig != "" {
							lastSignature = sig
							tc.ProviderMeta = map[string]string{"thought_signature": sig}
						}
						toolCalls = append(toolCalls, tc)
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

		// Tool calls are assembled from the streamed functionCall parts and
		// delivered on a terminal parcel (empty Data/Thinking) so the executor's
		// stream path can finalize them exactly like the non-streaming chat path.
		if len(toolCalls) > 0 {
			select {
			case parcels <- &modelrepo.StreamParcel{ToolCalls: toolCalls}:
			case <-ctx.Done():
				return
			}
		}

		reportChange("stream_completed", map[string]any{
			"chunk_count":     chunkCount,
			"total_length":    totalContent.Len(),
			"tool_call_count": len(toolCalls),
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
