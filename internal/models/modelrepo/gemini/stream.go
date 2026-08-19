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

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/google/uuid"
)

type GeminiStreamClient struct {
	geminiClient
}

// Stream emits raw deltas: text and thinking as they arrive, each functionCall
// part as one whole-call ToolCallDelta, then one terminal parcel. Assembly
// belongs to modelrepo.StreamAssembler.
func (c *GeminiStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	parcels := make(chan *modelrepo.StreamParcel)
	request, err := buildGeminiRequest(c.modelName, messages, args, c.canThink)
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
			send(&modelrepo.StreamParcel{Error: err})
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
			send(&modelrepo.StreamParcel{Error: err})
			return
		}
		defer resp.Body.Close()

		reportChange("gemini_stream_response", map[string]any{
			"status":  resp.StatusCode,
			"headers": resp.Header,
		})

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			err = fmt.Errorf("gemini API returned non-200 status for stream: %d, body: %s", resp.StatusCode, string(b))
			err = modelrepo.ClassifyProviderError(err, resp.StatusCode, "", string(b))
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}

		var (
			toolCallIndex int
			textLen       int
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

			var chunk geminiGenerateContentResponse
			if err := json.Unmarshal([]byte(jsonData), &chunk); err != nil {
				continue
			}

			if chunk.PromptFeedback.BlockReason != "" {
				err = fmt.Errorf("stream blocked by API for reason: %s", chunk.PromptFeedback.BlockReason)
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
					if !send(&modelrepo.StreamParcel{Thinking: part.Text}) {
						return
					}
				case part.Text != "":
					textLen += len(part.Text)
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

		// Same contract as the vertex stream: neither text nor a tool call is an
		// error, not an empty success, so the caller's retry fires (Gemini emits
		// this shape on MALFORMED_FUNCTION_CALL).
		if textLen == 0 && toolCallIndex == 0 {
			reason := finishReason
			if reason == "" {
				reason = "unknown"
			}
			err := fmt.Errorf("empty stream from Gemini model %s: finish reason (%s)", c.modelName, reason)
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}

		send(&modelrepo.StreamParcel{Terminal: &modelrepo.StreamTerminal{
			FinishReason: finishReason,
			Usage:        usage,
		}})
	}()

	return parcels, nil
}

var _ modelrepo.LLMStreamClient = (*GeminiStreamClient)(nil)
