package mistral

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

const defaultBaseURL = "https://api.mistral.ai/v1"

// mistralClient is the shared transport for the direct Mistral API
// (api.mistral.ai), which speaks the OpenAI-compatible chat/completions format.
type mistralClient struct {
	baseURL         string
	apiKey          string
	modelName       string
	maxOutputTokens int
	httpClient      *http.Client
	tracker         libtracker.ActivityTracker
}

func chatConfigFromArgs(args []modelrepo.ChatArgument) *modelrepo.ChatConfig {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}
	return cfg
}

func (c *mistralClient) url(path string) string {
	return strings.TrimRight(c.baseURL, "/") + path
}

func (c *mistralClient) post(ctx context.Context, path string, request any) ([]byte, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("mistral: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mistral: request failed for model %s: %w", c.modelName, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mistral API error: %d - %s (model=%s)", resp.StatusCode, strings.TrimSpace(string(body)), c.modelName)
	}
	return body, nil
}

func (c *mistralClient) openStream(ctx context.Context, path string, request any) (*http.Response, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("mistral: marshal stream request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mistral: stream request failed for model %s: %w", c.modelName, err)
	}
	if resp.StatusCode != http.StatusOK {
		bd, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("mistral API stream error: %d - %s", resp.StatusCode, strings.TrimSpace(string(bd)))
	}
	return resp, nil
}

type mistralChatClient struct{ mistralClient }

func (c *mistralChatClient) Chat(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (modelrepo.ChatResult, error) {
	reportErr, reportChange, end := c.tracker.Start(ctx, "chat", "mistral", "model", c.modelName)
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

type mistralStreamClient struct{ mistralClient }

func (c *mistralStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	cfg := chatConfigFromArgs(args)
	req, nameMap := chatcompletions.Build(c.modelName, messages, cfg)
	req.MaxTokens = modelrepo.ClampMaxOutputTokensPtr(req.MaxTokens, c.maxOutputTokens)
	req.Stream = true
	dec := chatcompletions.NewStreamDecoder(nameMap)

	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "mistral", "model", c.modelName)
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
			case parcels <- &modelrepo.StreamParcel{Error: fmt.Errorf("mistral: stream read: %w", err)}:
			case <-ctx.Done():
			}
			return
		}
		reportChange("stream_completed", map[string]any{"chunk_count": chunkCount})
	}()
	return parcels, nil
}

type mistralPromptClient struct{ mistralClient }

func (c *mistralPromptClient) Prompt(ctx context.Context, systemInstruction string, temperature float32, prompt string) (string, error) {
	msgs := []modelrepo.Message{{Role: "user", Content: prompt}}
	if s := strings.TrimSpace(systemInstruction); s != "" {
		msgs = append([]modelrepo.Message{{Role: "system", Content: s}}, msgs...)
	}
	chat := &mistralChatClient{mistralClient: c.mistralClient}
	res, err := chat.Chat(ctx, msgs, modelrepo.WithTemperature(float64(temperature)))
	if err != nil {
		return "", err
	}
	return res.Message.Content, nil
}

var (
	_ modelrepo.LLMChatClient       = (*mistralChatClient)(nil)
	_ modelrepo.LLMStreamClient     = (*mistralStreamClient)(nil)
	_ modelrepo.LLMPromptExecClient = (*mistralPromptClient)(nil)
)
