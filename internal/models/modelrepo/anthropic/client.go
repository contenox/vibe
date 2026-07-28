package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/kernel/reasoning"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	msgcodec "github.com/contenox/contenox/internal/models/modelrepo/codec/messages"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	// anthropicAPIVersion is the direct-API version header value (Vertex uses a
	// different value placed in the body; that path lives in the vertex package).
	anthropicAPIVersion = "2023-06-01"
)

// anthropicClient is the shared transport for the direct Anthropic API.
type anthropicClient struct {
	baseURL         string
	apiKey          string
	modelName       string
	httpClient      *http.Client
	canThink        bool
	maxOutputTokens int
	tracker         libtracker.ActivityTracker
}

func chatConfigFromArgs(args []modelrepo.ChatArgument) *modelrepo.ChatConfig {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}
	return cfg
}

func (c *anthropicClient) newRequest(ctx context.Context, path string, body []byte, stream bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+path, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return req, nil
}

func (c *anthropicClient) post(ctx context.Context, path string, request any) ([]byte, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	// Bounded end-to-end; retried on 429/529 (overloaded)/5xx with Retry-After honored.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()
	resp, err := modelrepo.DoWithRetry(ctx, c.httpClient, func() (*http.Request, error) {
		return c.newRequest(ctx, path, b, false)
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed for model %s: %w", c.modelName, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, anthropicAPIError(resp.StatusCode, body, c.modelName)
	}
	return body, nil
}

// anthropicAPIError classifies the response into a typed sentinel: 429 and 529
// (overloaded_error) are rate/capacity limits; a 400 invalid_request_error
// mentioning the context window is a context-length overflow.
func anthropicAPIError(status int, body []byte, modelName string) error {
	err := fmt.Errorf("anthropic API error: %d - %s (model=%s)", status, strings.TrimSpace(string(body)), modelName)
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	message := string(body)
	if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
		message = payload.Error.Message
	}
	return modelrepo.ClassifyProviderError(err, status, "", message)
}

func (c *anthropicClient) openStream(ctx context.Context, path string, request any) (*http.Response, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal stream request: %w", err)
	}
	req, err := c.newRequest(ctx, path, b, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: stream request failed for model %s: %w", c.modelName, err)
	}
	if resp.StatusCode != http.StatusOK {
		bd, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, anthropicAPIError(resp.StatusCode, bd, c.modelName)
	}
	return resp, nil
}

func applyAnthropicThinking(req *msgcodec.Request, modelName string, cfg *modelrepo.ChatConfig) {
	if req == nil || cfg == nil || cfg.Think == nil {
		return
	}
	level, ok, err := reasoning.NormalizeOptional(*cfg.Think)
	if err != nil || !ok || level == reasoning.Auto {
		return
	}
	if level == reasoning.Off {
		if !anthropicRequiresAdaptiveThinking(modelName) && !anthropicMythos(modelName) {
			req.Thinking = &msgcodec.ThinkingConfig{Type: "disabled"}
		}
		return
	}

	if anthropicUsesAdaptiveThinking(modelName) {
		req.Thinking = &msgcodec.ThinkingConfig{Type: "adaptive", Display: "summarized"}
		if effort := anthropicEffort(modelName, level); effort != "" && effort != reasoning.High {
			req.OutputConfig = &msgcodec.OutputConfig{Effort: effort}
		} else if effort == reasoning.High {
			// high is the API default; omit output_config to keep the payload smaller.
			req.OutputConfig = nil
		}
		return
	}

	budget := anthropicThinkingBudget(level, req.MaxTokens)
	if budget <= 0 {
		return
	}
	req.Thinking = &msgcodec.ThinkingConfig{Type: "enabled", BudgetTokens: budget}
}

// stripThinkingUnlessEnabled drops replayed thinking blocks whenever the
// outgoing request does not turn thinking on: Anthropic rejects history
// thinking blocks unless the request enables thinking (enabled/adaptive).
func stripThinkingUnlessEnabled(req *msgcodec.Request) {
	if req.Thinking == nil || req.Thinking.Type == "disabled" {
		msgcodec.StripThinkingBlocks(req)
	}
}

func anthropicThinkingBudget(level string, maxTokens int) int {
	budget := 0
	switch level {
	case reasoning.Minimal, reasoning.Low:
		budget = 1024
	case reasoning.Medium:
		budget = 2048
	case reasoning.High:
		budget = 4096
	case reasoning.XHigh:
		budget = 8192
	}
	if maxTokens > 1 && budget >= maxTokens {
		budget = maxTokens - 1
	}
	return budget
}

func anthropicEffort(modelName, level string) string {
	switch level {
	case reasoning.Minimal:
		return reasoning.Low
	case reasoning.Low, reasoning.Medium, reasoning.High:
		return level
	case reasoning.XHigh:
		if anthropicSupportsXHighEffort(modelName) {
			return reasoning.XHigh
		}
		return reasoning.High
	default:
		return ""
	}
}

func anthropicUsesAdaptiveThinking(modelName string) bool {
	m := strings.ToLower(modelName)
	return strings.Contains(m, "claude-opus-4-8") ||
		strings.Contains(m, "claude-opus-4-7") ||
		strings.Contains(m, "claude-opus-4-6") ||
		strings.Contains(m, "claude-sonnet-4-6") ||
		strings.Contains(m, "claude-fable-5") ||
		strings.Contains(m, "claude-sonnet-5") ||
		anthropicMythos(m)
}

func anthropicRequiresAdaptiveThinking(modelName string) bool {
	m := strings.ToLower(modelName)
	return strings.Contains(m, "claude-opus-4-8") ||
		strings.Contains(m, "claude-opus-4-7") ||
		strings.Contains(m, "claude-fable-5") ||
		strings.Contains(m, "claude-sonnet-5") ||
		anthropicMythos(m)
}

func anthropicSupportsXHighEffort(modelName string) bool {
	m := strings.ToLower(modelName)
	return strings.Contains(m, "claude-opus-4-8") ||
		strings.Contains(m, "claude-opus-4-7") ||
		strings.Contains(m, "claude-fable-5")
}

func anthropicStripsTemperatureParams(modelName string) bool {
	m := strings.ToLower(modelName)
	return strings.Contains(m, "claude-opus-4-7") ||
		strings.Contains(m, "claude-opus-4-8") ||
		strings.Contains(m, "claude-fable-5") ||
		strings.Contains(m, "claude-sonnet-5") ||
		anthropicMythos(m)
}

func anthropicMythos(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "mythos")
}

type anthropicChatClient struct{ anthropicClient }

func (c *anthropicChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "anthropic", "model", c.modelName)
	defer end()

	cfg := chatConfigFromArgs(args)
	req, nameMap := msgcodec.Build(messages, cfg)
	req.Model = c.modelName // direct: model in body, version via header
	if anthropicStripsTemperatureParams(c.modelName) {
		req.Temperature = nil
		req.TopP = nil
	}
	req.MaxTokens, _ = modelrepo.ClampMaxOutputTokens(req.MaxTokens, c.maxOutputTokens)
	if c.canThink {
		applyAnthropicThinking(&req, c.modelName, cfg)
	}
	stripThinkingUnlessEnabled(&req)

	raw, err := c.post(ctx, "/v1/messages", req)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	res, err := msgcodec.DecodeResponse(raw, nameMap)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	reportChange("chat_completed", res)
	return res, nil
}

type anthropicStreamClient struct{ anthropicClient }

func (c *anthropicStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	cfg := chatConfigFromArgs(args)
	req, nameMap := msgcodec.Build(messages, cfg)
	req.Model = c.modelName
	if anthropicStripsTemperatureParams(c.modelName) {
		req.Temperature = nil
		req.TopP = nil
	}
	req.MaxTokens, _ = modelrepo.ClampMaxOutputTokens(req.MaxTokens, c.maxOutputTokens)
	if c.canThink {
		applyAnthropicThinking(&req, c.modelName, cfg)
	}
	stripThinkingUnlessEnabled(&req)
	req.Stream = true
	dec := msgcodec.NewStreamDecoder(nameMap)

	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "anthropic", "model", c.modelName)
	resp, err := c.openStream(ctx, "/v1/messages", req)
	if err != nil {
		reportErr(err)
		end()
		return nil, err
	}

	parcels := make(chan *modelrepo.StreamParcel)
	go func() {
		defer close(parcels)
		defer resp.Body.Close()
		defer end()

		send := func(p *modelrepo.StreamParcel) bool {
			select {
			case parcels <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var chunkCount int
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue // skip SSE `event:` lines and blanks
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			ps, derr := dec.DecodeLine([]byte(payload))
			if derr != nil {
				// In-stream `error` events (overloaded_error etc.) surface here;
				// swallowing them would let the stream end looking successful.
				derr = fmt.Errorf("anthropic stream failed for model %s: %w", c.modelName, derr)
				reportErr(derr)
				send(&modelrepo.StreamParcel{Error: derr})
				return
			}
			for _, p := range ps {
				chunkCount++
				if !send(p) {
					return
				}
			}
		}
		if err := sc.Err(); err != nil && err != io.EOF {
			reportErr(err)
			send(&modelrepo.StreamParcel{Error: fmt.Errorf("anthropic: stream read: %w", err)})
			return
		}
		if !send(dec.Finish()) {
			return
		}
		reportChange("stream_completed", map[string]any{"chunk_count": chunkCount})
	}()
	return parcels, nil
}

type anthropicPromptClient struct{ anthropicClient }

func (c *anthropicPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, *modelrepo.TokenUsage, error) {
	msgs := []modelrepo.Message{{Role: "user", Content: prompt}}
	if s := strings.TrimSpace(systemInstruction); s != "" {
		msgs = append([]modelrepo.Message{{Role: "system", Content: s}}, msgs...)
	}
	chat := &anthropicChatClient{anthropicClient: c.anthropicClient}
	var chatArgs []modelrepo.ChatArgument
	if !anthropicStripsTemperatureParams(c.modelName) {
		chatArgs = append(chatArgs, modelrepo.WithTemperature(float64(temperature)))
	}
	res, err := chat.Chat(ctx, msgs, chatArgs...)
	if err != nil {
		return "", nil, err
	}
	return res.Message.Content, res.Usage, nil
}

var (
	_ modelrepo.LLMChatClient       = (*anthropicChatClient)(nil)
	_ modelrepo.LLMStreamClient     = (*anthropicStreamClient)(nil)
	_ modelrepo.LLMPromptExecClient = (*anthropicPromptClient)(nil)
)
