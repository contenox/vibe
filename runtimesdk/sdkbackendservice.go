package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/runtimetypes"
)

// HTTPBackendService implements the backendservice.Service interface
// using HTTP calls to the API
type HTTPBackendService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPBackendService creates a new HTTP client that implements backendservice.Service
func NewHTTPBackendService(baseURL, token string, client *http.Client) backendservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPBackendService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Create implements backendservice.Service.Create
func (s *HTTPBackendService) Create(ctx context.Context, backend *runtimetypes.Backend) error {
	rUrl := s.baseURL + "/backends"

	req, err := http.NewRequestWithContext(ctx, "POST", rUrl, nil)
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	// Encode request body
	body, err := json.Marshal(backend)
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
	if resp.StatusCode != http.StatusCreated {
		return apiframework.HandleAPIError(resp)
	}

	// Decode response into the provided backend
	if err := json.NewDecoder(resp.Body).Decode(backend); err != nil {
		return err
	}

	return nil
}

// Get implements backendservice.Service.Get
func (s *HTTPBackendService) Get(ctx context.Context, id string) (*runtimetypes.Backend, error) {
	url := fmt.Sprintf("%s/backends/%s", s.baseURL, id)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check for error status codes
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	// The API returns a RespBackend struct with additional fields
	var apiResponse struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		BaseURL   string    `json:"baseUrl"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return nil, err
	}

	// Convert to store.Backend
	backend := &runtimetypes.Backend{
		ID:        apiResponse.ID,
		Name:      apiResponse.Name,
		BaseURL:   apiResponse.BaseURL,
		Type:      apiResponse.Type,
		CreatedAt: apiResponse.CreatedAt,
		UpdatedAt: apiResponse.UpdatedAt,
	}

	return backend, nil
}

// Update implements backendservice.Service.Update
func (s *HTTPBackendService) Update(ctx context.Context, backend *runtimetypes.Backend) error {
	url := fmt.Sprintf("%s/backends/%s", s.baseURL, backend.ID)

	req, err := http.NewRequestWithContext(ctx, "PUT", url, nil)
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Encode request body
	body, err := json.Marshal(backend)
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

	// Decode response into the provided backend
	if err := json.NewDecoder(resp.Body).Decode(backend); err != nil {
		return err
	}

	return nil
}

// Delete implements backendservice.Service.Delete
func (s *HTTPBackendService) Delete(ctx context.Context, id string) error {
	url := fmt.Sprintf("%s/backends/%s", s.baseURL, id)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

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

// List implements backendservice.Service.List
func (s *HTTPBackendService) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Backend, error) {
	url := fmt.Sprintf("%s/backends?limit=%d", s.baseURL, limit)
	if createdAtCursor != nil {
		url += "&cursor=" + createdAtCursor.Format(time.RFC3339Nano)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check for error status codes
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	// The API returns a slice of respBackendList structs with additional fields
	var apiResponses []struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		BaseURL   string    `json:"baseUrl"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResponses); err != nil {
		return nil, err
	}

	// Convert to []*store.Backend
	backends := make([]*runtimetypes.Backend, 0, len(apiResponses))
	for _, apiResp := range apiResponses {
		backends = append(backends, &runtimetypes.Backend{
			ID:        apiResp.ID,
			Name:      apiResp.Name,
			BaseURL:   apiResp.BaseURL,
			Type:      apiResp.Type,
			CreatedAt: apiResp.CreatedAt,
			UpdatedAt: apiResp.UpdatedAt,
		})
	}

	return backends, nil
}
