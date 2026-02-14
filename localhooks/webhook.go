package localhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/contenox/vibe/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// WebCaller makes HTTP requests to external services
type WebCaller struct {
	client         *http.Client
	defaultHeaders map[string]string
}

// NewWebCaller creates a new webhook caller
func NewWebCaller(options ...WebhookOption) taskengine.HookRepo {
	wh := &WebCaller{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		defaultHeaders: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
		},
	}

	for _, opt := range options {
		opt(wh)
	}

	return wh
}

// WebhookOption configures the WebhookCaller
type WebhookOption func(*WebCaller)

// WithHTTPClient sets a custom HTTP client
func WithHTTPClient(client *http.Client) WebhookOption {
	return func(h *WebCaller) {
		h.client = client
	}
}

// WithDefaultHeader sets a default header
func WithDefaultHeader(key, value string) WebhookOption {
	return func(h *WebCaller) {
		h.defaultHeaders[key] = value
	}
}

// Exec implements the HookRepo interface
func (h *WebCaller) Exec(ctx context.Context, startTime time.Time, input any, debug bool, hook *taskengine.HookCall) (any, taskengine.DataType, error) {
	// Get URL from args
	rawURL, ok := hook.Args["url"]
	if !ok {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missing 'url' argument")
	}

	// Parse URL
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("invalid URL: %w", err)
	}

	// Handle query parameters
	if queryParams, ok := hook.Args["query"]; ok {
		params, err := url.ParseQuery(queryParams)
		if err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("invalid query parameters: %w", err)
		}
		baseURL.RawQuery = params.Encode()
	}

	// Determine HTTP method
	method := "POST"
	if m, ok := hook.Args["method"]; ok {
		method = m
	}
	if method == "POST" && input == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("missing input for POST request")
	}

	// Prepare request body
	var body io.Reader
	if method == "POST" {
		switch v := input.(type) {
		case string:
			// If input is JSON, send as-is
			if json.Valid([]byte(v)) {
				body = bytes.NewBufferString(v)
			} else {
				// Otherwise wrap in JSON
				payload := map[string]interface{}{
					"message": v,
					"data":    hook.Args,
				}
				jsonData, err := json.Marshal(payload)
				if err != nil {
					return nil, taskengine.DataTypeAny, fmt.Errorf("failed to marshal payload: %w", err)
				}
				body = bytes.NewBuffer(jsonData)
			}
		default:
			// For non-string input, marshal to JSON
			jsonData, err := json.Marshal(input)
			if err != nil {
				return nil, taskengine.DataTypeAny, fmt.Errorf("failed to marshal input: %w", err)
			}
			body = bytes.NewBuffer(jsonData)
		}
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, method, baseURL.String(), body)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	for k, v := range h.defaultHeaders {
		req.Header.Set(k, v)
	}
	if headers, ok := hook.Args["headers"]; ok {
		var headerMap map[string]string
		if err := json.Unmarshal([]byte(headers), &headerMap); err == nil {
			for k, v := range headerMap {
				req.Header.Set(k, v)
			}
		}
	}

	// Make the request
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for success status (2xx)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var result interface{}
		if err := json.Unmarshal(respBody, &result); err == nil {
			return result, taskengine.DataTypeJSON, nil
		}
		return string(respBody), taskengine.DataTypeString, nil
	}

	return nil, taskengine.DataTypeAny, fmt.Errorf("webhook failed with status %d: %s", resp.StatusCode, string(respBody))
}

// Supports returns the hook types supported by this hook.
func (h *WebCaller) Supports(ctx context.Context) ([]string, error) {
	return []string{"webhook"}, nil
}

// GetSchemasForSupportedHooks returns OpenAPI schemas for supported hooks.
func (h *WebCaller) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	// WebCaller doesn't have a static schema as it calls arbitrary endpoints
	return nil, nil
}

// GetToolsForHookByName returns tools exposed by this hook.
func (h *WebCaller) GetToolsForHookByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != "webhook" {
		return nil, fmt.Errorf("unknown hook: %s", name)
	}

	return []taskengine.Tool{
		{
			Type: "function",
			Function: taskengine.FunctionTool{
				Name:        "webhook",
				Description: "Makes HTTP requests to external services",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url": map[string]interface{}{
							"type":        "string",
							"description": "The URL to call",
						},
						"method": map[string]interface{}{
							"type":        "string",
							"description": "HTTP method (GET, POST, etc.)",
							"enum":        []string{"GET", "POST", "PUT", "PATCH", "DELETE"},
						},
						"headers": map[string]interface{}{
							"type":        "object",
							"description": "HTTP headers to include",
						},
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Query parameters",
						},
					},
					"required": []string{"url"},
				},
			},
		},
	}, nil
}

var _ taskengine.HookRepo = (*WebCaller)(nil)
