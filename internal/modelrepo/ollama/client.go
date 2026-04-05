package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/ollama/ollama/api"
)

const ollamaMaxStreamBuffer = 8 * 1024 * 1024

type ollamaHTTPClient struct {
	baseURL    *url.URL
	httpClient *http.Client
	apiKey     string
}

func newOllamaHTTPClient(baseURL, apiKey string, httpClient *http.Client) (*ollamaHTTPClient, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("invalid ollama base URL %q: %w", baseURL, err)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api")
	if u.Path == "/" {
		u.Path = ""
	}
	return &ollamaHTTPClient{
		baseURL:    u,
		httpClient: httpClient,
		apiKey:     strings.TrimSpace(apiKey),
	}, nil
}

func (c *ollamaHTTPClient) endpointURL(endpoint string) string {
	endpoint = "/" + strings.TrimLeft(endpoint, "/")
	return c.baseURL.JoinPath("/api" + endpoint).String()
}

func (c *ollamaHTTPClient) newRequest(ctx context.Context, method, endpoint string, body io.Reader, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpointURL(endpoint), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

func (c *ollamaHTTPClient) do(ctx context.Context, method, endpoint string, request any, response any) error {
	var body io.Reader
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}

	req, err := c.newRequest(ctx, method, endpoint, body, "application/json")
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := ollamaAPIError(resp.StatusCode, raw); err != nil {
		return err
	}
	if len(raw) > 0 && response != nil {
		return json.Unmarshal(raw, response)
	}
	return nil
}

func (c *ollamaHTTPClient) stream(ctx context.Context, method, endpoint string, request any, onChunk func([]byte) error) error {
	var body io.Reader
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}

	req, err := c.newRequest(ctx, method, endpoint, body, "application/x-ndjson")
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		return ollamaAPIError(resp.StatusCode, raw)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), ollamaMaxStreamBuffer)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := onChunk(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (c *ollamaHTTPClient) Chat(ctx context.Context, req *api.ChatRequest, fn func(api.ChatResponse) error) error {
	return c.stream(ctx, http.MethodPost, "/chat", req, func(line []byte) error {
		var resp api.ChatResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		return fn(resp)
	})
}

func (c *ollamaHTTPClient) Generate(ctx context.Context, req *api.GenerateRequest, fn func(api.GenerateResponse) error) error {
	return c.stream(ctx, http.MethodPost, "/generate", req, func(line []byte) error {
		var resp api.GenerateResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		return fn(resp)
	})
}

func (c *ollamaHTTPClient) Embed(ctx context.Context, req *api.EmbedRequest) (*api.EmbedResponse, error) {
	var resp api.EmbedResponse
	if err := c.do(ctx, http.MethodPost, "/embed", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *ollamaHTTPClient) List(ctx context.Context) (*api.ListResponse, error) {
	var resp api.ListResponse
	if err := c.do(ctx, http.MethodGet, "/tags", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *ollamaHTTPClient) Show(ctx context.Context, req *api.ShowRequest) (*api.ShowResponse, error) {
	var resp api.ShowResponse
	if err := c.do(ctx, http.MethodPost, "/show", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *ollamaHTTPClient) Delete(ctx context.Context, req *api.DeleteRequest) error {
	return c.do(ctx, http.MethodDelete, "/delete", req, nil)
}

func ollamaAPIError(status int, raw []byte) error {
	if status < http.StatusBadRequest {
		return nil
	}

	var body struct {
		Error     string `json:"error"`
		SigninURL string `json:"signin_url,omitempty"`
	}
	_ = json.Unmarshal(raw, &body)

	msg := strings.TrimSpace(body.Error)
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if body.SigninURL != "" {
		msg = fmt.Sprintf("%s (signin: %s)", msg, body.SigninURL)
	}
	return fmt.Errorf("ollama API returned %d: %s", status, msg)
}

func buildOllamaOptions(config *modelrepo.ChatConfig) map[string]any {
	opts := make(map[string]any)
	if config.Temperature != nil {
		opts["temperature"] = *config.Temperature
	}
	if config.MaxTokens != nil {
		opts["num_predict"] = *config.MaxTokens
	}
	if config.TopP != nil {
		opts["top_p"] = *config.TopP
	}
	if config.Seed != nil {
		opts["seed"] = *config.Seed
	}
	return opts
}

func buildOllamaThink(config *modelrepo.ChatConfig) api.ThinkValue {
	think := api.ThinkValue{Value: false}
	if config.Think == nil {
		return think
	}
	switch strings.ToLower(strings.TrimSpace(*config.Think)) {
	case "true", "high", "medium", "low":
		think = api.ThinkValue{Value: strings.ToLower(strings.TrimSpace(*config.Think))}
	case "false", "none":
		think = api.ThinkValue{Value: false}
	}
	return think
}

func buildOllamaTools(config *modelrepo.ChatConfig) (api.Tools, error) {
	if len(config.Tools) == 0 {
		return nil, nil
	}

	apiTools := make(api.Tools, 0, len(config.Tools))
	for _, tool := range config.Tools {
		if tool.Type == "" || tool.Function == nil || tool.Function.Name == "" {
			continue
		}

		var params api.ToolFunctionParameters
		if tool.Function.Parameters != nil {
			raw, err := json.Marshal(tool.Function.Parameters)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal tool parameters for %s: %w", tool.Function.Name, err)
			}
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, fmt.Errorf("failed to unmarshal tool parameters for %s: %w", tool.Function.Name, err)
			}
		}

		apiTools = append(apiTools, api.Tool{
			Type: tool.Type,
			Function: api.ToolFunction{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  params,
			},
		})
	}
	return apiTools, nil
}
