package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/embedservice"
)

// HTTPEmbedService implements the embedservice.Service interface
// using HTTP calls to the API
type HTTPEmbedService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPEmbedService creates a new HTTP client that implements embedservice.Service
func NewHTTPEmbedService(baseURL, token string, client *http.Client) embedservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPEmbedService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Embed implements embedservice.Service.Embed
func (s *HTTPEmbedService) Embed(ctx context.Context, text string) ([]float64, error) {
	url := s.baseURL + "/embed"

	// Create request body
	reqBody := struct {
		Text string `json:"text"`
	}{Text: text}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(strings.NewReader(string(bodyBytes))))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	// Parse response
	var response struct {
		Vector []float64 `json:"vector"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return response.Vector, nil
}

// DefaultModelName implements embedservice.Service.
func (s *HTTPEmbedService) DefaultModelName(ctx context.Context) (string, error) {
	url := s.baseURL + "/defaultmodel"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return "", apiframework.HandleAPIError(resp)
	}

	// Parse response
	var response struct {
		ModelName string `json:"modelName"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return response.ModelName, nil
}
