package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
)

// HTTPTaskChainService implements the taskchainservice.Service interface
// using HTTP calls to the API
type HTTPTaskChainService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPTaskChainService creates a new HTTP client that implements taskchainservice.Service
func NewHTTPTaskChainService(baseURL, token string, client *http.Client) taskchainservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPTaskChainService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Create implements taskchainservice.Service.Create
func (s *HTTPTaskChainService) Create(ctx context.Context, chain *taskengine.TaskChainDefinition) error {
	url := s.baseURL + "/taskchains"

	// Marshal the chain definition
	body, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("failed to marshal task chain: %w", err)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Execute request
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Handle non-201 responses
	if resp.StatusCode != http.StatusCreated {
		return apiframework.HandleAPIError(resp)
	}

	// Decode response into the provided chain (to get updated fields)
	if err := json.NewDecoder(resp.Body).Decode(chain); err != nil {
		return fmt.Errorf("failed to decode task chain response: %w", err)
	}

	return nil
}

// Get implements taskchainservice.Service.Get
func (s *HTTPTaskChainService) Get(ctx context.Context, id string) (*taskengine.TaskChainDefinition, error) {
	if id == "" {
		return nil, fmt.Errorf("task chain ID is required")
	}

	url := fmt.Sprintf("%s/taskchains/%s", s.baseURL, url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Execute request
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	// Decode response
	var chain taskengine.TaskChainDefinition
	if err := json.NewDecoder(resp.Body).Decode(&chain); err != nil {
		return nil, fmt.Errorf("failed to decode task chain response: %w", err)
	}

	return &chain, nil
}

// Update implements taskchainservice.Service.Update
func (s *HTTPTaskChainService) Update(ctx context.Context, chain *taskengine.TaskChainDefinition) error {
	if chain.ID == "" {
		return fmt.Errorf("task chain ID is required")
	}

	url := fmt.Sprintf("%s/taskchains/%s", s.baseURL, url.PathEscape(chain.ID))
	body, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("failed to marshal task chain: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Execute request
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	// Update chain with server response
	if err := json.NewDecoder(resp.Body).Decode(chain); err != nil {
		return fmt.Errorf("failed to decode updated task chain: %w", err)
	}

	return nil
}

// Delete implements taskchainservice.Service.Delete
func (s *HTTPTaskChainService) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("task chain ID is required")
	}

	url := fmt.Sprintf("%s/taskchains/%s", s.baseURL, url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Execute request
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}

// List implements taskchainservice.Service.List
func (s *HTTPTaskChainService) List(ctx context.Context, cursor *time.Time, limit int) ([]*taskengine.TaskChainDefinition, error) {
	// Build URL with query parameters
	rUrl := fmt.Sprintf("%s/taskchains?limit=%d", s.baseURL, limit)
	if cursor != nil {
		rUrl += "&cursor=" + url.QueryEscape(cursor.Format(time.RFC3339Nano))
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rUrl, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Execute request
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	// Decode response
	var chains []*taskengine.TaskChainDefinition
	if err := json.NewDecoder(resp.Body).Decode(&chains); err != nil {
		return nil, fmt.Errorf("failed to decode task chains response: %w", err)
	}

	return chains, nil
}
