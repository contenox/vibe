package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/modelrepo/codec/chatcompletions"
)

type orClient struct {
	baseURL         string
	apiKey          string
	modelName       string
	maxOutputTokens int
	httpClient      *http.Client
	tracker         libtracker.ActivityTracker
}

func (c *orClient) url(path string) string {
	return strings.TrimRight(c.baseURL, "/") + path
}

func (c *orClient) post(ctx context.Context, path string, request any) ([]byte, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("openrouter: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: request failed for model %s: %w", c.modelName, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter API error: %d - %s (model=%s)", resp.StatusCode, strings.TrimSpace(string(body)), c.modelName)
	}
	return body, nil
}

func (c *orClient) openStream(ctx context.Context, path string, request any) (*http.Response, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("openrouter: marshal stream request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: stream request failed for model %s: %w", c.modelName, err)
	}
	if resp.StatusCode != http.StatusOK {
		bd, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openrouter API stream error: %d - %s", resp.StatusCode, strings.TrimSpace(string(bd)))
	}
	return resp, nil
}

func chatConfigFromArgs(args []modelrepo.ChatArgument) *modelrepo.ChatConfig {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}
	return cfg
}

// orChatClient implements modelrepo.LLMChatClient.
type orChatClient struct{ orClient }

func (c *orChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "openrouter", "model", c.modelName)
	defer end()

	cfg := chatConfigFromArgs(args)
	req, nameMap := chatcompletions.Build(c.modelName, messages, cfg)
	req.MaxTokens = modelrepo.ClampMaxOutputTokensPtr(req.MaxTokens, c.maxOutputTokens)
	raw, err := c.post(ctx, "/chat/completions", req)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	res, err := chatcompletions.DecodeResponse(raw, nameMap)
	if err != nil {
		reportErr(err)
		return modelrepo.ChatResult{}, err
	}
	reportChange("chat_completed", res)
	return res, nil
}

// orStreamClient implements modelrepo.LLMStreamClient.
type orStreamClient struct{ orClient }

func (c *orStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	cfg := chatConfigFromArgs(args)
	req, nameMap := chatcompletions.Build(c.modelName, messages, cfg)
	req.MaxTokens = modelrepo.ClampMaxOutputTokensPtr(req.MaxTokens, c.maxOutputTokens)
	req.Stream = true
	dec := chatcompletions.NewStreamDecoder(nameMap)

	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "openrouter", "model", c.modelName)
	resp, err := c.openStream(ctx, "/chat/completions", req)
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
		sc := bufio.NewScanner(resp.Body)
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
			p, derr := dec.DecodeLine([]byte(payload))
			if derr != nil {
				continue
			}
			if p != nil {
				chunkCount++
				select {
				case parcels <- p:
				case <-ctx.Done():
					return
				}
			}
		}
		if err := sc.Err(); err != nil && err != io.EOF {
			reportErr(err)
			select {
			case parcels <- &modelrepo.StreamParcel{Error: fmt.Errorf("openrouter: stream read: %w", err)}:
			case <-ctx.Done():
			}
			return
		}
		reportChange("stream_completed", map[string]any{"chunk_count": chunkCount})
	}()
	return parcels, nil
}

// orPromptClient implements modelrepo.LLMPromptExecClient.
type orPromptClient struct{ orClient }

func (c *orPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	msgs := []modelrepo.Message{{Role: "user", Content: prompt}}
	if s := strings.TrimSpace(systemInstruction); s != "" {
		msgs = append([]modelrepo.Message{{Role: "system", Content: s}}, msgs...)
	}
	chat := &orChatClient{orClient: c.orClient}
	res, err := chat.Chat(ctx, msgs, modelrepo.WithTemperature(float64(temperature)))
	if err != nil {
		return "", err
	}
	return res.Message.Content, nil
}

var (
	_ modelrepo.LLMChatClient       = (*orChatClient)(nil)
	_ modelrepo.LLMStreamClient     = (*orStreamClient)(nil)
	_ modelrepo.LLMPromptExecClient = (*orPromptClient)(nil)
)
