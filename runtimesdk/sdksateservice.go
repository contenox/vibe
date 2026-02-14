package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/stateservice"
	"github.com/contenox/vibe/statetype"
)

// HTTPStateService implements the stateservice.Service interface
// using HTTP calls to the API
type HTTPStateService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPStateService creates a new HTTP client that implements stateservice.Service
func NewHTTPStateService(baseURL, token string, client *http.Client) stateservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPStateService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Get implements modelservice.Service.Get
func (h *HTTPStateService) Get(ctx context.Context) ([]statetype.BackendRuntimeState, error) {
	// Build URL with query parameters
	rUrl := fmt.Sprintf("%s/state", h.baseURL)

	req, err := http.NewRequestWithContext(ctx, "GET", rUrl, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	if h.token != "" {
		req.Header.Set("X-API-Key", h.token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check for error status codes
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var response []statetype.BackendRuntimeState
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}

	return response, nil
}
