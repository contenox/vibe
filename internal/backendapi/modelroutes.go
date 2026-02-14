package backendapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/downloadservice"
	"github.com/contenox/vibe/modelservice"
	"github.com/contenox/vibe/runtimetypes"
)

func AddModelRoutes(mux *http.ServeMux, modelService modelservice.Service, dwService downloadservice.Service) {
	s := &service{service: modelService, dwService: dwService}

	mux.HandleFunc("POST /models", s.createModel)
	mux.HandleFunc("GET /openai/v1/models", s.listModels)
	mux.HandleFunc("GET /openai/{chainID}/v1/models", s.listModels)
	mux.HandleFunc("PUT /models/{id}", s.updateModel)
	mux.HandleFunc("GET /models", s.listInternal)
	// mux.HandleFunc("GET /v1/models/{model}", s.modelDetails) // TODO: Implement model details endpoint
	mux.HandleFunc("DELETE /models/{model}", s.deleteModel)
}

type service struct {
	service   modelservice.Service
	dwService downloadservice.Service
}

// Declares a new model to the system.
//
// The model must be available in a configured backend or will be queued for download.
// IMPORTANT: Models not assigned to any group will NOT be available for request processing.
// If groups are enabled, to make a model available to backends, it must be explicitly added to at least one group.
func (s *service) createModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	model, err := serverops.Decode[runtimetypes.Model](r) // @request runtimetypes.Model
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	model.ID = model.Model
	if err := s.service.Append(ctx, &model); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, model) // @response runtimetypes.Model
}

type OpenAIModel struct {
	ID      string `json:"id" example:"mistral:latest"`
	Object  string `json:"object" example:"mistral:latest"`
	Created int64  `json:"created" example:"1717020800"`
	OwnedBy string `json:"owned_by" example:"system"`
}

type OpenAICompatibleModelList struct {
	Object string        `json:"object" example:"list"`
	Data   []OpenAIModel `json:"data"`
}

// Lists all registered models in OpenAI-compatible format.
//
// Returns models as they would appear in OpenAI's /v1/models endpoint.
// NOTE: Only models assigned to at least one group will be available for request processing.
// Models not assigned to any group exist in the configuration but are completely ignored by the routing system.
// the chainID parameter is currently unused.
func (s *service) listModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse pagination parameters using the helper
	limitStr := serverops.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := serverops.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")
	_ = serverops.GetPathParam(r, "chainID", "The ID of the chain that links to the openAI completion API. Currently unused.")
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

	limit := 100 // Default limit
	if limitStr != "" {
		i, err := strconv.Atoi(limitStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid limit format, expected integer", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		limit = i
	}

	// Get internal models with pagination
	internalModels, err := s.service.List(ctx, cursor, limit)
	if err != nil {
		serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	openAIModels := make([]OpenAIModel, len(internalModels))

	for i, m := range internalModels {
		openAIModels[i] = OpenAIModel{
			ID:      m.Model,
			Object:  "model",
			Created: m.CreatedAt.Unix(),
			OwnedBy: "system",
		}
	}

	response := OpenAICompatibleModelList{
		Object: "list",
		Data:   openAIModels,
	}

	serverops.Encode(w, r, http.StatusOK, response) // @response backendapi.OpenAICompatibleModelList
}

// Updates an existing model registration.
//
// Only mutable fields (like capabilities and context length) can be updated.
// The model ID cannot be changed.
// Returns the updated model configuration.
func (s *service) updateModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Extract and validate model ID from path
	id := serverops.GetPathParam(r, "id", "The unique identifier for the model.")
	if id == "" {
		_ = serverops.Error(w, r, fmt.Errorf("model ID is required: %w", serverops.ErrBadPathValue), serverops.UpdateOperation)
		return
	}

	// Decode request body into Model struct
	updatedModel, err := serverops.Decode[runtimetypes.Model](r) // @request runtimetypes.Model
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	// Ensure the ID in the URL matches the model data (if present)
	if updatedModel.ID != "" && updatedModel.ID != id {
		err = fmt.Errorf("%w: ID in payload does not match URL", serverops.ErrUnprocessableEntity)
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}
	updatedModel.ID = id // enforce ID from URL

	// Perform update
	if err := s.service.Update(ctx, &updatedModel); err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	// Return updated model
	_ = serverops.Encode(w, r, http.StatusOK, updatedModel) // @response runtimetypes.Model
}

// Lists all registered models in internal format.
//
// This endpoint returns full model details including timestamps and capabilities.
// Intended for administrative and debugging purposes.
func (s *service) listInternal(w http.ResponseWriter, r *http.Request) {
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

	limit := 100
	if limitStr != "" {
		i, err := strconv.Atoi(limitStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid limit format, expected integer", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		if i < 1 {
			err = fmt.Errorf("%w: limit must be positive", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		limit = i
	}

	// Reuse the same service.List method that returns internal models
	models, err := s.service.List(ctx, cursor, limit)
	if err != nil {
		serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	// Return raw internal models
	_ = serverops.Encode(w, r, http.StatusOK, models) // @response []*runtimetypes.Model
}

// Deletes a model from the system registry.
//
// - Does not remove the model from backend storage (requires separate backend operation)
// - Accepts 'purge=true' query parameter to also remove related downloads from queue
func (s *service) deleteModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelName := serverops.GetPathParam(r, "model", "The name of the model to delete (e.g., 'mistral:latest').")
	if modelName == "" {
		serverops.Error(w, r, fmt.Errorf("model name is required: %w", serverops.ErrBadPathValue), serverops.DeleteOperation)
		return
	}
	if err := s.service.Delete(ctx, modelName); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}
	purgeQueue := serverops.GetQueryParam(r, "purge", "false", "If true, also removes the model from the download queue and cancels any in-progress downloads.")
	if purgeQueue == "true" {
		if err := s.dwService.RemoveDownloadFromQueue(r.Context(), modelName); err != nil {
			_ = serverops.Error(w, r, err, serverops.DeleteOperation)
			return
		}
		if err := s.dwService.CancelDownloads(r.Context(), modelName); err != nil {
			_ = serverops.Error(w, r, err, serverops.DeleteOperation)
			return
		}
	}

	_ = serverops.Encode(w, r, http.StatusOK, "model removed") // @response string
}
