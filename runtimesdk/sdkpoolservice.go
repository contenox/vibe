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

	"github.com/contenox/vibe/affinitygroupservice"
	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/runtimetypes"
)

// HTTPgroupService implements the groupservice.Service interface
// using HTTP calls to the API
type HTTPgroupService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPgroupService creates a new HTTP client that implements groupservice.Service
func NewHTTPgroupService(baseURL, token string, client *http.Client) affinitygroupservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPgroupService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// Create implements groupservice.Service.Create
func (s *HTTPgroupService) Create(ctx context.Context, group *runtimetypes.AffinityGroup) error {
	url := s.baseURL + "/groups"

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
	body, err := json.Marshal(group)
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

	// Decode response into the provided group
	if err := json.NewDecoder(resp.Body).Decode(group); err != nil {
		return err
	}

	return nil
}

// GetByID implements groupservice.Service.GetByID
func (s *HTTPgroupService) GetByID(ctx context.Context, id string) (*runtimetypes.AffinityGroup, error) {
	url := fmt.Sprintf("%s/groups/%s", s.baseURL, url.PathEscape(id))

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

	// Decode response
	var group runtimetypes.AffinityGroup
	if err := json.NewDecoder(resp.Body).Decode(&group); err != nil {
		return nil, err
	}

	return &group, nil
}

// GetByName implements groupservice.Service.GetByName
func (s *HTTPgroupService) GetByName(ctx context.Context, name string) (*runtimetypes.AffinityGroup, error) {
	url := fmt.Sprintf("%s/group-by-name/%s", s.baseURL, url.PathEscape(name))

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

	// Decode response
	var group runtimetypes.AffinityGroup
	if err := json.NewDecoder(resp.Body).Decode(&group); err != nil {
		return nil, err
	}

	return &group, nil
}

// Update implements groupservice.Service.Update
func (s *HTTPgroupService) Update(ctx context.Context, group *runtimetypes.AffinityGroup) error {
	url := fmt.Sprintf("%s/groups/%s", s.baseURL, url.PathEscape(group.ID))

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
	body, err := json.Marshal(group)
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

	// Decode response into the provided group
	if err := json.NewDecoder(resp.Body).Decode(group); err != nil {
		return err
	}

	return nil
}

// Delete implements groupservice.Service.Delete
func (s *HTTPgroupService) Delete(ctx context.Context, id string) error {
	url := fmt.Sprintf("%s/groups/%s", s.baseURL, url.PathEscape(id))

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
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}

// ListAll implements groupservice.Service.ListAll
func (s *HTTPgroupService) ListAll(ctx context.Context) ([]*runtimetypes.AffinityGroup, error) {
	url := s.baseURL + "/groups"

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

	// Decode response
	var groups []*runtimetypes.AffinityGroup
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, err
	}

	return groups, nil
}

// ListByPurpose implements groupservice.Service.ListByPurpose
func (s *HTTPgroupService) ListByPurpose(ctx context.Context, purpose string, createdAtCursor *time.Time, limit int) ([]*runtimetypes.AffinityGroup, error) {
	// Build URL with query parameters
	rUrl := fmt.Sprintf("%s/group-by-purpose/%s?limit=%d", s.baseURL, url.PathEscape(purpose), limit)
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

	// Decode response
	var groups []*runtimetypes.AffinityGroup
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, err
	}

	return groups, nil
}

// AssignBackend implements groupservice.Service.AssignBackend
func (s *HTTPgroupService) AssignBackend(ctx context.Context, groupID, backendID string) error {
	url := fmt.Sprintf("%s/backend-affinity/%s/backends/%s",
		s.baseURL, url.PathEscape(groupID), url.PathEscape(backendID))

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
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
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	return nil
}

// RemoveBackend implements groupservice.Service.RemoveBackend
func (s *HTTPgroupService) RemoveBackend(ctx context.Context, groupID, backendID string) error {
	url := fmt.Sprintf("%s/backend-affinity/%s/backends/%s",
		s.baseURL, url.PathEscape(groupID), url.PathEscape(backendID))

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

// ListBackends implements groupservice.Service.ListBackends
func (s *HTTPgroupService) ListBackends(ctx context.Context, groupID string) ([]*runtimetypes.Backend, error) {
	url := fmt.Sprintf("%s/backend-affinity/%s/backends", s.baseURL, url.PathEscape(groupID))

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

	// Decode response
	var backends []*runtimetypes.Backend
	if err := json.NewDecoder(resp.Body).Decode(&backends); err != nil {
		return nil, err
	}

	return backends, nil
}

// ListAffinityGroupsForBackend implements groupservice.Service.ListAffinityGroupsForBackend
func (s *HTTPgroupService) ListAffinityGroupsForBackend(ctx context.Context, backendID string) ([]*runtimetypes.AffinityGroup, error) {
	url := fmt.Sprintf("%s/backend-affinity/%s/groups", s.baseURL, url.PathEscape(backendID))

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

	// Decode response
	var groups []*runtimetypes.AffinityGroup
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, err
	}

	return groups, nil
}

// AssignModel implements groupservice.Service.AssignModel
func (s *HTTPgroupService) AssignModel(ctx context.Context, groupID, modelID string) error {
	url := fmt.Sprintf("%s/model-affinity/%s/models/%s",
		s.baseURL, url.PathEscape(groupID), url.PathEscape(modelID))

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
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

// RemoveModel implements groupservice.Service.RemoveModel
func (s *HTTPgroupService) RemoveModel(ctx context.Context, groupID, modelID string) error {
	url := fmt.Sprintf("%s/model-affinity/%s/models/%s",
		s.baseURL, url.PathEscape(groupID), url.PathEscape(modelID))

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

// ListModels implements groupservice.Service.ListModels
func (s *HTTPgroupService) ListModels(ctx context.Context, groupID string) ([]*runtimetypes.Model, error) {
	url := fmt.Sprintf("%s/model-affinity/%s/models", s.baseURL, url.PathEscape(groupID))

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

	// Decode response
	var models []*runtimetypes.Model
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, err
	}

	return models, nil
}

// ListAffinityGroupsForModel implements groupservice.Service.ListAffinityGroupsForModel
func (s *HTTPgroupService) ListAffinityGroupsForModel(ctx context.Context, modelID string) ([]*runtimetypes.AffinityGroup, error) {
	url := fmt.Sprintf("%s/model-affinity/%s/groups", s.baseURL, url.PathEscape(modelID))

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

	// Decode response
	var groups []*runtimetypes.AffinityGroup
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, err
	}

	return groups, nil
}
