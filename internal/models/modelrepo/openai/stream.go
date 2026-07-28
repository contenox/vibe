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

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/codec/chatcompletions"
)

type OpenAIStreamClient struct {
	openAIClient
}

// Stream emits raw deltas per the modelrepo.StreamParcel contract: content /
// thinking / tool-call fragments as they arrive on the wire, then one typed
// terminal parcel (finish reason + usage). Assembly belongs to the engine-side
// modelrepo.StreamAssembler, never to this client.
func (c *OpenAIStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "openai", "model", c.modelName)
	// end() is not deferred here; ownership passes to the goroutine below since the stream is asynchronous.

	streamCh := make(chan *modelrepo.StreamParcel)
	usesResponses := openAIUsesResponsesEndpoint(c.modelName)
	endpoint := "/chat/completions"
	var requestBody []byte
	var nameMap map[string]string
	var err error

	if usesResponses {
		var req openAIResponsesRequest
		req, nameMap = buildOpenAIResponsesRequestWithCapabilities(c.modelName, messages, args, c.supportsThink)
		c.clampResponsesMaxOutputTokens(&req)
		req.Stream = true
		requestBody, err = json.Marshal(req)
		endpoint = "/responses"
	} else {
		var req openAIChatRequest
		req, nameMap = buildOpenAIRequestWithCapabilities(c.modelName, messages, args, c.supportsThink)
		req.Stream = true
		req.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
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

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		err = fmt.Errorf("OpenAI API returned non-200 status: %d - %s for model %s",
			resp.StatusCode, string(body), c.modelName)
		err = modelrepo.ClassifyProviderError(err, resp.StatusCode, "", string(body))
		reportErr(err)
		end()
		return nil, err
	}

	go func() {
		defer close(streamCh)
		defer resp.Body.Close()
		defer end()

		if usesResponses {
			streamResponsesSSE(ctx, resp.Body, nameMap, streamCh, reportErr, reportChange)
			return
		}

		send := func(p *modelrepo.StreamParcel) bool {
			select {
			case streamCh <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}

		dec := chatcompletions.NewStreamDecoder(nameMap)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var chunkCount int

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			jsonData := strings.TrimPrefix(line, "data: ")
			if jsonData == "[DONE]" {
				continue
			}
			parcels, derr := dec.DecodeLine([]byte(jsonData))
			if derr != nil {
				derr = fmt.Errorf("failed to decode SSE data: %w, raw: %s", derr, jsonData)
				reportErr(derr)
				send(&modelrepo.StreamParcel{Error: derr})
				return
			}
			for _, p := range parcels {
				chunkCount++
				if !send(p) {
					return
				}
			}
		}

		if err := scanner.Err(); err != nil && err != io.EOF {
			err = fmt.Errorf("stream scanning error: %w", err)
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}

		if !send(dec.Finish()) {
			return
		}
		reportChange("stream_completed", map[string]any{"chunk_count": chunkCount})
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
	// response.output_text.delta / response.reasoning_summary_text.delta
	Delta string `json:"delta"`
	// output slot of the item the event belongs to; groups tool-call fragments.
	OutputIndex int `json:"output_index"`
	// response.output_item.added / .done — the (partial) item
	Item *openAIResponseOutputItem `json:"item"`
	// response.completed — the full response (usage + reasoning summary)
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

// streamResponsesSSE reads a Responses API SSE stream and forwards raw-delta
// parcels to out: text/reasoning deltas as they arrive, tool-call fragments as
// ToolCallDelta parcels keyed by output_index (id/name from
// response.output_item.added, args from response.function_call_arguments.delta),
// and a typed terminal parcel from response.completed.
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
	var emittedReasoning bool

	send := func(p *modelrepo.StreamParcel) bool {
		select {
		case out <- p:
			return true
		case <-ctx.Done():
			return false
		}
	}

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
			if !send(&modelrepo.StreamParcel{Data: ev.Delta}) {
				return
			}

		case "response.reasoning_summary_text.delta":
			if ev.Delta == "" {
				continue
			}
			emittedReasoning = true
			if !send(&modelrepo.StreamParcel{Thinking: ev.Delta}) {
				return
			}

		case "response.output_item.added":
			// A function_call item opens a tool-call slot: id + name arrive
			// here, the argument fragments follow as separate delta events.
			if ev.Item == nil || strings.ToLower(ev.Item.Type) != "function_call" {
				continue
			}
			id := ev.Item.CallID
			if id == "" {
				id = ev.Item.ID
			}
			name := ev.Item.Name
			if orig, ok := nameMap[name]; ok && orig != "" {
				name = orig
			}
			if !send(&modelrepo.StreamParcel{ToolCall: &modelrepo.ToolCallDelta{
				Index: ev.OutputIndex,
				ID:    id,
				Type:  "function",
				Name:  name,
			}}) {
				return
			}

		case "response.function_call_arguments.delta":
			if ev.Delta == "" {
				continue
			}
			if !send(&modelrepo.StreamParcel{ToolCall: &modelrepo.ToolCallDelta{
				Index:        ev.OutputIndex,
				ArgsFragment: ev.Delta,
			}}) {
				return
			}

		case "response.completed":
			// Fallback for gateways that never emitted reasoning-summary deltas.
			if !emittedReasoning {
				if summary := responsesReasoningSummaryText(ev.Response); summary != "" {
					if !send(&modelrepo.StreamParcel{Thinking: summary}) {
						return
					}
				}
			}
			term := &modelrepo.StreamTerminal{FinishReason: "stop"}
			if ev.Response != nil {
				term.Usage = ev.Response.Usage.neutralUsage()
			}
			if !send(&modelrepo.StreamParcel{Terminal: term}) {
				return
			}
			reportChange("stream_completed", map[string]any{
				"path":        "responses",
				"chunk_count": chunkCount,
			})
			return

		case "error":
			err := fmt.Errorf("responses stream error %s", ev.errorText(payload))
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return

		case "response.failed", "response.incomplete":
			// Without this, the stream would end silently as an empty completion.
			err := fmt.Errorf("responses stream %s %s", ev.Type, ev.errorText(payload))
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: err})
			return
		}
	}

	if err := sc.Err(); err != nil && err != io.EOF {
		err = fmt.Errorf("responses: stream read: %w", err)
		reportErr(err)
		send(&modelrepo.StreamParcel{Error: err})
		return
	}

	// The stream ended without response.completed/failed — surface it instead
	// of letting a truncated connection read as success.
	err := fmt.Errorf("responses: stream ended without response.completed")
	reportErr(err)
	send(&modelrepo.StreamParcel{Error: err})
}

var _ modelrepo.LLMStreamClient = (*OpenAIStreamClient)(nil)
