package runtimesdk

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/executor"
)

// HTTPExecutorSyncTrigger implements executor.ExecutorSyncTrigger
// using HTTP calls to the API
type HTTPExecutorSyncTrigger struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPExecutorSyncTrigger creates a new HTTP client for triggering executor sync
func NewHTTPExecutorSyncTrigger(baseURL, token string, client *http.Client) executor.ExecutorSyncTrigger {
	if client == nil {
		client = http.DefaultClient
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &HTTPExecutorSyncTrigger{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// TriggerSync implements executor.ExecutorSyncTrigger.TriggerSync
func (s *HTTPExecutorSyncTrigger) TriggerSync() {
	_ = s.triggerSyncInternal(context.Background())
}

// triggerSyncInternal is a helper that allows error handling and context usage
func (s *HTTPExecutorSyncTrigger) triggerSyncInternal(ctx context.Context) error {
	url := s.baseURL + "/executor/sync"

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}
