package backendapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/backendservice"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/stateservice"
	"github.com/contenox/vibe/statetype"
	"github.com/google/uuid"
)

func AddBackendRoutes(mux *http.ServeMux, backendService backendservice.Service, stateService stateservice.Service) {
	b := &backendManager{service: backendService, stateService: stateService}

	mux.HandleFunc("POST /backends", b.createBackend)
	mux.HandleFunc("GET /backends", b.listBackends)
	mux.HandleFunc("GET /backends/{id}", b.getBackend)
	mux.HandleFunc("PUT /backends/{id}", b.updateBackend)
	mux.HandleFunc("DELETE /backends/{id}", b.deleteBackend)
}

type backendSummary struct {
	ID      string `json:"id" example:"backend-id"`
	Name    string `json:"name" example:"backend-name"`
	BaseURL string `json:"baseUrl" example:"http://localhost:11434"`
	Type    string `json:"type" example:"ollama"`

	Models       []string                    `json:"models"`
	PulledModels []statetype.ModelPullStatus `json:"pulledModels" openapi_include_type:"statetype.ModelPullStatus"`
	Error        string                      `json:"error,omitempty" example:"error-message"`

	CreatedAt time.Time `json:"createdAt" example:"2023-01-01T00:00:00Z"`
	UpdatedAt time.Time `json:"updatedAt" example:"2023-01-01T00:00:00Z"`
}

type backendManager struct {
	service      backendservice.Service
	stateService stateservice.Service
}

// Creates a new backend connection to an LLM provider.
//
// Backends represent connections to LLM services (e.g., Ollama, OpenAI) that can host models.
// Note: Creating a backend will be provisioned on the next synchronization cycle.
func (b *backendManager) createBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	backend, err := serverops.Decode[runtimetypes.Backend](r) // @request runtimetypes.Backend
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}
	backend.ID = uuid.NewString()
	if err := b.service.Create(ctx, &backend); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, backend) // @response runtimetypes.Backend
}

// Lists all configured backend connections with runtime status.
//
// NOTE: Only backends assigned to at least one group will be used for request processing.
// Backends not assigned to any group exist in the configuration but are completely ignored by the routing system.
func (b *backendManager) listBackends(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse pagination parameters using the helper
	limitStr := serverops.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := serverops.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	var cursor *time.Time
	if cursorStr != "" {
		t, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid cursor format, expected RFC3339Nano", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		cursor = &t
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		err = fmt.Errorf("%w: invalid limit format, expected integer", serverops.ErrUnprocessableEntity)
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	backends, err := b.service.List(ctx, cursor, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	backendState, err := b.stateService.Get(ctx)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	resp := []backendSummary{}
	for _, backend := range backends {
		item := backendSummary{
			ID:      backend.ID,
			Name:    backend.Name,
			BaseURL: backend.BaseURL,
			Type:    backend.Type,
		}
		ok := false
		var itemState statetype.BackendRuntimeState
		for _, l := range backendState {
			if l.ID == backend.ID {
				ok = true

				itemState = l
				break
			}
		}
		if ok {
			item.Models = itemState.Models
			item.PulledModels = itemState.PulledModels
			item.Error = itemState.Error
		}
		resp = append(resp, item)
	}

	_ = serverops.Encode(w, r, http.StatusOK, resp) // @response []backendapi.backendSummary
}

type backendDetails struct {
	ID           string                      `json:"id" example:"b7d9e1a3-8f0c-4a7d-9b1e-2f3a4b5c6d7e"`
	Name         string                      `json:"name" example:"ollama-production"`
	BaseURL      string                      `json:"baseUrl" example:"http://ollama-prod.internal:11434"`
	Type         string                      `json:"type" example:"ollama"`
	Models       []string                    `json:"models" example:"[\"mistral:instruct\", \"llama2:7b\", \"nomic-embed-text:latest\"]"`
	PulledModels []statetype.ModelPullStatus `json:"pulledModels" openapi_include_type:"statetype.ModelPullStatus"`
	Error        string                      `json:"error,omitempty" example:"connection timeout: context deadline exceeded"`
	CreatedAt    time.Time                   `json:"createdAt" example:"2023-11-15T14:30:45Z"`
	UpdatedAt    time.Time                   `json:"updatedAt" example:"2023-11-15T14:30:45Z"`
}

// Retrieves complete information for a specific backend
func (b *backendManager) getBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := serverops.GetPathParam(r, "id", "The unique identifier for the backend.")
	if id == "" {
		serverops.Error(w, r, fmt.Errorf("missing id parameter %w", serverops.ErrBadPathValue), serverops.GetOperation)
		return
	}

	// Get static backend info
	backend, err := b.service.Get(ctx, id)
	if err != nil {
		serverops.Error(w, r, err, serverops.GetOperation)
		return
	}

	// Get dynamic runtime state
	state, err := b.stateService.Get(ctx)
	if err != nil {
		serverops.Error(w, r, err, serverops.GetOperation)
		return
	}
	ok := false
	var itemState statetype.BackendRuntimeState
	for _, l := range state {
		if l.ID == id {
			ok = true

			itemState = l
			break
		}
	}

	resp := backendDetails{
		ID:           backend.ID,
		Name:         backend.Name,
		BaseURL:      backend.BaseURL,
		Type:         backend.Type,
		Models:       []string{},
		PulledModels: []statetype.ModelPullStatus{},
		Error:        "",
		CreatedAt:    backend.CreatedAt,
		UpdatedAt:    backend.UpdatedAt,
	}

	if ok {
		resp.Models = itemState.Models
		resp.PulledModels = itemState.PulledModels
		resp.Error = itemState.Error
	}

	serverops.Encode(w, r, http.StatusOK, resp) // @response backendapi.backendDetails
}

// Updates an existing backend configuration.
//
// The ID from the URL path overrides any ID in the request body.
// Note: Updating a backend will be provisioned on the next synchronization cycle.
func (b *backendManager) updateBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := serverops.GetPathParam(r, "id", "The unique identifier for the backend.")
	if id == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing id parameter %w", serverops.ErrBadPathValue), serverops.UpdateOperation)
		return
	}
	backend, err := serverops.Decode[runtimetypes.Backend](r) // @request runtimetypes.Backend
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	backend.ID = id
	if err := b.service.Update(ctx, &backend); err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, backend) // @response runtimetypes.Backend
}

// Removes a backend connection.
//
// This does not deleteBackend models from the remote provider, only removes the connection.
// Returns a simple "backend removed" confirmation message on success.
func (b *backendManager) deleteBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := serverops.GetPathParam(r, "id", "The unique identifier for the backend.")
	if id == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing id parameter %w", serverops.ErrBadPathValue), serverops.DeleteOperation)
		return
	}
	if err := b.service.Delete(ctx, id); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "backend removed") // @response string
}
