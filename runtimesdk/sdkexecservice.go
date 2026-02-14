package runtimesdk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/execservice"
)

// HTTPExecService implements the execservice.ExecService interface
// using HTTP calls to the API
type HTTPExecService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPExecService creates a new HTTP client that implements execservice.ExecService
func NewHTTPExecService(baseURL, token string, client *http.Client) execservice.ExecService {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPExecService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Execute implements execservice.ExecService.Execute
func (s *HTTPExecService) Execute(ctx context.Context, request *execservice.TaskRequest) (*execservice.SimpleExecutionResponse, error) {
	url := s.baseURL + "/execute"

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	// Encode request body
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(strings.NewReader(string(body)))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check for error status codes
	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	// Decode response
	var taskResponse execservice.SimpleExecutionResponse
	if err := json.NewDecoder(resp.Body).Decode(&taskResponse); err != nil {
		return nil, err
	}

	return &taskResponse, nil
}
