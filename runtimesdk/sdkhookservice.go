package runtimesdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/hookproviderservice"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/getkin/kin-openapi/openapi3"
)

// HTTPRemoteHookService implements the hookproviderservice.Service interface
// using HTTP calls to the API.
type HTTPRemoteHookService struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPRemoteHookService creates a new HTTP client that implements hookproviderservice.Service.
func NewHTTPRemoteHookService(baseURL, token string, client *http.Client) hookproviderservice.Service {
	if client == nil {
		client = http.DefaultClient
	}

	// Ensure baseURL doesn't end with a slash
	baseURL = strings.TrimSuffix(baseURL, "/")

	return &HTTPRemoteHookService{
		client:  client,
		baseURL: baseURL,
		token:   token,
	}
}

// NEW: ListLocalHooks implements hookproviderservice.Service.ListLocalHooks.
func (s *HTTPRemoteHookService) ListLocalHooks(ctx context.Context) ([]hookproviderservice.LocalHook, error) {
	reqUrl := s.baseURL + "/hooks/local"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
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

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var localHooks []hookproviderservice.LocalHook
	if err := json.NewDecoder(resp.Body).Decode(&localHooks); err != nil {
		return nil, err
	}

	return localHooks, nil
}

// GetSchemasForSupportedHooks implements hookproviderservice.Service.GetSchemasForSupportedHooks.
func (s *HTTPRemoteHookService) GetSchemasForSupportedHooks(ctx context.Context) (map[string]*openapi3.T, error) {
	reqUrl := s.baseURL + "/hooks/schemas"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
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

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var schemas map[string]*openapi3.T
	if err := json.NewDecoder(resp.Body).Decode(&schemas); err != nil {
		return nil, err
	}

	return schemas, nil
}

// Create implements hookproviderservice.Service.Create.
func (s *HTTPRemoteHookService) Create(ctx context.Context, hook *runtimetypes.RemoteHook) error {
	reqUrl := s.baseURL + "/hooks/remote"

	body, err := json.Marshal(hook)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqUrl, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return apiframework.HandleAPIError(resp)
	}

	if err := json.NewDecoder(resp.Body).Decode(hook); err != nil {
		return err
	}

	return nil
}

// Get implements hookproviderservice.Service.Get.
func (s *HTTPRemoteHookService) Get(ctx context.Context, id string) (*runtimetypes.RemoteHook, error) {
	reqUrl := fmt.Sprintf("%s/hooks/remote/%s", s.baseURL, url.PathEscape(id))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
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

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var hook runtimetypes.RemoteHook
	if err := json.NewDecoder(resp.Body).Decode(&hook); err != nil {
		return nil, err
	}

	return &hook, nil
}

// GetByName implements hookproviderservice.Service.GetByName.
func (s *HTTPRemoteHookService) GetByName(ctx context.Context, name string) (*runtimetypes.RemoteHook, error) {
	reqUrl := fmt.Sprintf("%s/hooks/remote/by-name/%s", s.baseURL, url.PathEscape(name))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqUrl, nil)
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

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var hook runtimetypes.RemoteHook
	if err := json.NewDecoder(resp.Body).Decode(&hook); err != nil {
		return nil, err
	}

	return &hook, nil
}

// Update implements hookproviderservice.Service.Update.
func (s *HTTPRemoteHookService) Update(ctx context.Context, hook *runtimetypes.RemoteHook) error {
	reqUrl := fmt.Sprintf("%s/hooks/remote/%s", s.baseURL, url.PathEscape(hook.ID))

	body, err := json.Marshal(hook)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqUrl, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiframework.HandleAPIError(resp)
	}

	if err := json.NewDecoder(resp.Body).Decode(hook); err != nil {
		return err
	}

	return nil
}

// Delete implements hookproviderservice.Service.Delete.
func (s *HTTPRemoteHookService) Delete(ctx context.Context, id string) error {
	reqUrl := fmt.Sprintf("%s/hooks/remote/%s", s.baseURL, url.PathEscape(id))

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqUrl, nil)
	if err != nil {
		return err
	}

	if s.token != "" {
		req.Header.Set("X-API-Key", s.token)
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

// List implements hookproviderservice.Service.List.
func (s *HTTPRemoteHookService) List(ctx context.Context, createdAtCursor *time.Time, limit int) ([]*runtimetypes.RemoteHook, error) {
	u, err := url.Parse(s.baseURL + "/hooks/remote")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("limit", fmt.Sprintf("%d", limit))
	if createdAtCursor != nil {
		q.Set("cursor", createdAtCursor.Format(time.RFC3339Nano))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
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

	if resp.StatusCode != http.StatusOK {
		return nil, apiframework.HandleAPIError(resp)
	}

	var hooks []*runtimetypes.RemoteHook
	if err := json.NewDecoder(resp.Body).Decode(&hooks); err != nil {
		return nil, err
	}

	return hooks, nil
}
