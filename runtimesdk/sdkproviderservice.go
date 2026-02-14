package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/providerservice"
)

// HTTPProviderService implements the providerservice.Service interface
// using HTTP calls to the API
type HTTPProviderService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPProviderService creates a new HTTP client that implements providerservice.Service
func NewHTTPProviderService(baseURL, token string, client *http.Client) providerservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPProviderService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// SetProviderConfig implements providerservice.Service.SetProviderConfig
func (s *HTTPProviderService) SetProviderConfig(ctx context.Context, providerType string, upsert bool, config *runtimestate.ProviderConfig) error {
	// Validate provider type
	if providerType != "openai" && providerType != "gemini" {
		return fmt.Errorf("invalid provider type: %s", providerType)
	}

	url := fmt.Sprintf("%s/providers/%s/configure", s.baseURL, providerType)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Encode request body
	requestBody := struct {
		APIKey string `json:"apiKey"`
		Upsert bool   `json:"upsert"`
	}{
		APIKey: config.APIKey,
		Upsert: upsert,
	}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(strings.NewReader(string(body)))

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check for error status codes
	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}

// GetProviderConfig implements providerservice.Service.GetProviderConfig
func (s *HTTPProviderService) GetProviderConfig(ctx context.Context, providerType string) (*runtimestate.ProviderConfig, error) {
	if providerType != "openai" && providerType != "gemini" {
		return nil, fmt.Errorf("invalid provider type: %s", providerType)
	}

	url := fmt.Sprintf("%s/providers/%s/config", s.baseURL, providerType)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("provider config not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var config runtimestate.ProviderConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// DeleteProviderConfig implements providerservice.Service.DeleteProviderConfig
func (s *HTTPProviderService) DeleteProviderConfig(ctx context.Context, providerType string) error {
	if providerType != "openai" && providerType != "gemini" {
		return fmt.Errorf("invalid provider type: %s", providerType)
	}

	url := fmt.Sprintf("%s/providers/%s/config", s.baseURL, providerType)
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

// ListProviderConfigs implements providerservice.Service.ListProviderConfigs
func (s *HTTPProviderService) ListProviderConfigs(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimestate.ProviderConfig, error) {
	u, err := url.Parse(s.baseURL + "/providers/configs")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if createdAtCursor != nil {
		q.Set("cursor", createdAtCursor.Format(time.RFC3339Nano))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
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

	var configs []*runtimestate.ProviderConfig
	if err := json.NewDecoder(resp.Body).Decode(&configs); err != nil {
		return nil, err
	}

	return configs, nil
}
