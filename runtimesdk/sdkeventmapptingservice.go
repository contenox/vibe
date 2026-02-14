package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/eventmappingservice"
	"github.com/contenox/vibe/eventstore"
)

// HTTPMappingService implements the eventmappingservice.Service interface
// using HTTP calls to the mapping API.
type HTTPMappingService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPMappingService creates a new HTTP client that implements eventmappingservice.Service.
func NewHTTPMappingService(baseURL, token string, client *http.Client) eventmappingservice.Service {
	if client == nil {
		client = http.DefaultClient
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &HTTPMappingService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// CreateMapping implements eventmappingservice.Service.CreateMapping.
func (s *HTTPMappingService) CreateMapping(ctx context.Context, config *eventstore.MappingConfig) error {
	url := s.baseURL + "/mappings"

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	body, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal mapping config: %w", err)
	}
	req.Body = io.NopCloser(strings.NewReader(string(body)))

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return apiframework.HandleAPIError(resp)
	}

	// Update config with any server-assigned fields (if any)
	return json.NewDecoder(resp.Body).Decode(config)
}

// GetMapping implements eventmappingservice.Service.GetMapping.
func (s *HTTPMappingService) GetMapping(ctx context.Context, path string) (*eventstore.MappingConfig, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}

	url := fmt.Sprintf("%s/mapping?path=%s", s.baseURL, url.QueryEscape(path))

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var config eventstore.MappingConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to decode mapping config: %w", err)
	}

	return &config, nil
}

// UpdateMapping implements eventmappingservice.Service.UpdateMapping.
func (s *HTTPMappingService) UpdateMapping(ctx context.Context, config *eventstore.MappingConfig) error {
	if config == nil || config.Path == "" {
		return fmt.Errorf("config and path are required")
	}

	url := fmt.Sprintf("%s/mapping?path=%s", s.baseURL, url.QueryEscape(config.Path))

	req, err := http.NewRequestWithContext(ctx, "PUT", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	body, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal mapping config: %w", err)
	}
	req.Body = io.NopCloser(strings.NewReader(string(body)))

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	// Update config with any server-assigned fields (if any)
	return json.NewDecoder(resp.Body).Decode(config)
}

// DeleteMapping implements eventmappingservice.Service.DeleteMapping.
func (s *HTTPMappingService) DeleteMapping(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}

	url := fmt.Sprintf("%s/mapping?path=%s", s.baseURL, url.QueryEscape(path))

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}

// ListMappings implements eventmappingservice.Service.ListMappings.
func (s *HTTPMappingService) ListMappings(ctx context.Context) ([]*eventstore.MappingConfig, error) {
	url := s.baseURL + "/mappings"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var configs []*eventstore.MappingConfig
	if err := json.NewDecoder(resp.Body).Decode(&configs); err != nil {
		return nil, fmt.Errorf("failed to decode mapping configs: %w", err)
	}

	return configs, nil
}
