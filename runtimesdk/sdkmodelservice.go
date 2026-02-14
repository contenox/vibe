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
	"github.com/contenox/vibe/modelservice"
	"github.com/contenox/vibe/runtimetypes"
)

// HTTPModelService implements the modelservice.Service interface
// using HTTP calls to the API
type HTTPModelService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPModelService creates a new HTTP client that implements modelservice.Service
func NewHTTPModelService(baseURL, token string, client *http.Client) modelservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPModelService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Append implements modelservice.Service.Append
func (s *HTTPModelService) Append(ctx context.Context, model *runtimetypes.Model) error {
	url := s.baseURL + "/models"

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	// Encode request body
	body, err := json.Marshal(model)
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

	// Decode response into the provided model
	// Note: API sets model.ID = model.Model, so we'll get the ID back
	if err := json.NewDecoder(resp.Body).Decode(model); err != nil {
		return err
	}

	return nil
}

// Update implements modelservice.Service.Update
func (s *HTTPModelService) Update(ctx context.Context, data *runtimetypes.Model) error {
	if data.ID == "" {
		return fmt.Errorf("model ID is required to update")
	}

	// Construct URL using model ID
	url := fmt.Sprintf("%s/models/%s", s.baseURL, url.PathEscape(data.ID))

	// Marshal the model data to JSON
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal model data: %w", err)
	}

	// Create request
	req, err := http.NewRequestWithContext(ctx, "PUT", url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	// Perform request
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Handle non-200 responses
	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	// On success, decode the response (full updated model) into data
	if err := json.NewDecoder(resp.Body).Decode(data); err != nil {
		return fmt.Errorf("failed to decode updated model response: %w", err)
	}

	return nil
}

// List implements modelservice.Service.List
// Uses the /models endpoint to get full model details
func (s *HTTPModelService) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.Model, error) {
	// Build URL for internal endpoint
	rUrl := fmt.Sprintf("%s/models?limit=%d", s.baseURL, limit)
	if createdAtCursor != nil {
		rUrl += "&cursor=" + url.QueryEscape(createdAtCursor.Format(time.RFC3339Nano))
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rUrl, nil)
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

	// Decode directly into []*runtimetypes.Model
	var models []*runtimetypes.Model
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("failed to decode internal models response: %w", err)
	}

	return models, nil
}

// Delete implements modelservice.Service.Delete
func (s *HTTPModelService) Delete(ctx context.Context, modelName string) error {
	// Properly escape the model name for the URL path
	url := fmt.Sprintf("%s/models/%s", s.baseURL, url.PathEscape(modelName))

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
