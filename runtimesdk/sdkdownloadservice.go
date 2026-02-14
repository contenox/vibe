package runtimesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/downloadservice"
	"github.com/contenox/vibe/runtimetypes"
)

// HTTPDownloadService implements the downloadservice.Service interface
// using HTTP calls to the API
type HTTPDownloadService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPDownloadService creates a new HTTP client that implements downloadservice.Service
func NewHTTPDownloadService(baseURL, token string, client *http.Client) downloadservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPDownloadService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// CurrentDownloadQueueState implements downloadservice.Service.CurrentDownloadQueueState
func (s *HTTPDownloadService) CurrentDownloadQueueState(ctx context.Context) ([]downloadservice.Job, error) {
	rUrl := s.baseURL + "/queue"

	req, err := http.NewRequestWithContext(ctx, "GET", rUrl, nil)
	if err != nil {
		return nil, err
	}

	// Set headers
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
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

	// Decode response
	var jobs []downloadservice.Job
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return nil, err
	}

	return jobs, nil
}

// CancelDownloads implements downloadservice.Service.CancelDownloads
func (s *HTTPDownloadService) CancelDownloads(ctx context.Context, urlParam string) error {
	// Properly encode the URL parameter
	encodedURL := url.QueryEscape(urlParam)

	url := fmt.Sprintf("%s/queue/cancel?url=%s", s.baseURL, encodedURL)

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

// RemoveDownloadFromQueue implements downloadservice.Service.RemoveDownloadFromQueue
func (s *HTTPDownloadService) RemoveDownloadFromQueue(ctx context.Context, modelName string) error {
	url := fmt.Sprintf("%s/queue/%s", s.baseURL, url.PathEscape(modelName))

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

// DownloadInProgress implements downloadservice.Service.DownloadInProgress
func (s *HTTPDownloadService) DownloadInProgress(ctx context.Context, statusCh chan<- *runtimetypes.Status) error {
	url := s.baseURL + "/queue/inProgress"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	// Set headers for SSE
	req.Header.Set("Accept", "text/event-stream")
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

	// Process the SSE stream
	reader := bufio.NewReader(resp.Body)
	for {
		// Check if context is canceled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Read line by line
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("error reading SSE stream: %w", err)
		}

		// Skip empty lines
		if len(line) <= 1 {
			continue
		}

		// Process "data:" lines
		if strings.HasPrefix(string(line), "data: ") {
			// Extract JSON data (remove "data: " prefix and trailing newline)
			jsonData := line[6 : len(line)-1]

			// Parse JSON into store.Status
			var status runtimetypes.Status
			if err := json.Unmarshal(jsonData, &status); err != nil {
				// Skip malformed events but continue processing the stream
				continue
			}

			// Send to channel if possible, otherwise skip
			select {
			case statusCh <- &status:
			default:
				// Channel is full, skip this update
			}
		}
	}
}
