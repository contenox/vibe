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

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/google/uuid"
)

type vertexStreamClient struct {
	vertexClient
}

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

		send := func(p *modelrepo.StreamParcel) bool {
			select {
			case parcels <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}

		body, err := json.Marshal(request)
		if err != nil {
			send(&modelrepo.StreamParcel{Error: fmt.Errorf("failed to marshal stream request: %w", err)})
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
			send(&modelrepo.StreamParcel{Error: err})
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
			send(&modelrepo.StreamParcel{Error: err})
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
			send(&modelrepo.StreamParcel{Error: err})
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
			err = modelrepo.ClassifyProviderError(err, resp.StatusCode, "", string(b))
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}

		var (
			chunkCount    int
			totalContent  strings.Builder
			toolCallIndex int
			lastSignature string
			finishReason  string
			usage         *modelrepo.TokenUsage
		)

		sc := bufio.NewScanner(resp.Body)
		// SSE frames carry a whole chunk per line; the bufio.Scanner default
		// 64KB cap truncates large chunks, so raise it.
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
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
				send(&modelrepo.StreamParcel{Error: err})
				return
			}
			if chunk.UsageMetadata != nil {
				usage = chunk.UsageMetadata.neutralUsage()
			}
			if len(chunk.Candidates) == 0 {
				continue
			}
			cand := chunk.Candidates[0]
			if cand.FinishReason != "" {
				finishReason = cand.FinishReason
			}
			for _, part := range cand.Content.Parts {
				switch {
				case part.Thought && part.Text != "":
					chunkCount++
					if !send(&modelrepo.StreamParcel{Thinking: part.Text}) {
						return
					}
				case part.Text != "":
					chunkCount++
					totalContent.WriteString(part.Text)
					if !send(&modelrepo.StreamParcel{Data: part.Text}) {
						return
					}
				case part.FunctionCall != nil:
					argsJSON, err := json.Marshal(part.FunctionCall.Args)
					if err != nil {
						continue
					}
					delta := &modelrepo.ToolCallDelta{
						Index:        toolCallIndex,
						ID:           uuid.NewString(),
						Type:         "function",
						Name:         part.FunctionCall.Name,
						ArgsFragment: string(argsJSON),
					}
					toolCallIndex++
					sig := part.ThoughtSignature
					if sig == "" {
						sig = part.FunctionCall.ThoughtSignature
					}
					if sig == "" {
						sig = lastSignature
					}
					if sig != "" {
						lastSignature = sig
						delta.ProviderMeta = map[string]string{"thought_signature": sig}
					}
					if !send(&modelrepo.StreamParcel{ToolCall: delta}) {
						return
					}
				}
			}
		}

		if err := sc.Err(); err != nil && err != io.EOF {
			err = fmt.Errorf("error reading from stream: %w", err)
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}

		if !send(&modelrepo.StreamParcel{Terminal: &modelrepo.StreamTerminal{
			FinishReason: finishReason,
			Usage:        usage,
		}}) {
			return
		}

		reportChange("stream_completed", map[string]any{
			"chunk_count":     chunkCount,
			"total_length":    totalContent.Len(),
			"tool_call_count": toolCallIndex,
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
