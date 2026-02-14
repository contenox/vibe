package groupapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/contenox/vibe/affinitygroupservice"
	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/runtimetypes"
)

func AddgroupRoutes(mux *http.ServeMux, groupService affinitygroupservice.Service) {
	s := &groupHandler{service: groupService}

	mux.HandleFunc("POST /groups", s.createAffinityGroup)
	mux.HandleFunc("GET /groups", s.listAffinityGroups)
	mux.HandleFunc("GET /groups/{id}", s.getAffinityGroup)
	mux.HandleFunc("PUT /groups/{id}", s.updateAffinityGroup)
	mux.HandleFunc("DELETE /groups/{id}", s.deleteAffinityGroup)
	mux.HandleFunc("GET /group-by-name/{name}", s.getAffinityGroupByName)
	mux.HandleFunc("GET /group-by-purpose/{purpose}", s.listAffinityGroupsByPurpose)

	// Backend associations
	mux.HandleFunc("POST /backend-affinity/{groupID}/backends/{backendID}", s.assignBackend)
	mux.HandleFunc("DELETE /backend-affinity/{groupID}/backends/{backendID}", s.removeBackend)
	mux.HandleFunc("GET /backend-affinity/{groupID}/backends", s.listBackendsByGroup)
	mux.HandleFunc("GET /backend-affinity/{backendID}/groups", s.listAffinityGroupsForBackend)

	// Model associations
	mux.HandleFunc("POST /model-affinity/{groupID}/models/{modelID}", s.assignModelToAffinityGroup)
	mux.HandleFunc("DELETE /model-affinity/{groupID}/models/{modelID}", s.removeModelFromAffinityGroup)
	mux.HandleFunc("GET /model-affinity/{groupID}/models", s.listModelsByAffinityGroup)
	mux.HandleFunc("GET /model-affinity/{modelID}/groups", s.listAffinityGroupsForModel)
}

type groupHandler struct {
	service affinitygroupservice.Service
}

// Creates a new affinity group for organizing backends and models.
//
// group names must be unique within the system.
// groups allow grouping of backends and models for specific operational purposes (e.g., embeddings, tasks).
//
// When affinity groups are enabled in the system, request routing ONLY considers resources that share a affinity group.
// - Models not assigned to any group will NOT be available for execution
// - Backends not assigned to any group will NOT receive models or process requests
// - Resources must be explicitly associated with the same group to work together
// This is a fundamental operational requirement - resources outside groups are effectively invisible to the routing system.
func (h *groupHandler) createAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	group, err := apiframework.Decode[runtimetypes.AffinityGroup](r) // @request runtimetypes.AffinityGroup
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	if err := h.service.Create(ctx, &group); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusCreated, group) // @response runtimetypes.AffinityGroup
}

// Retrieves an specific affinity group by its unique ID.
func (h *groupHandler) getAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the affinity group.")
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("id required: %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}

	group, err := h.service.GetByID(ctx, id)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, group) // @response runtimetypes.AffinityGroup
}

// Retrieves an affinity group by its human-readable name.
// Useful for configuration where ID might not be known but name is consistent.
func (h *groupHandler) getAffinityGroupByName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := apiframework.GetPathParam(r, "name", "The unique, human-readable name of the affinity group.")
	if name == "" {
		apiframework.Error(w, r, fmt.Errorf("id required: %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}

	group, err := h.service.GetByName(ctx, name)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, group) // @response runtimetypes.AffinityGroup
}

// Updates an existing affinity group configuration.
//
// The ID from the URL path overrides any ID in the request body.
func (h *groupHandler) updateAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the group to be updated.")
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("id required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	group, err := apiframework.Decode[runtimetypes.AffinityGroup](r) // @request runtimetypes.AffinityGroup
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	group.ID = id

	if err := h.service.Update(ctx, &group); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, group) // @response runtimetypes.AffinityGroup
}

// Removes an affinity group from the system.
//
// This does not delete the group's backends or models, only the group relationship.
// Returns a simple "deleted" confirmation message on success.
func (h *groupHandler) deleteAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the group to be deleted.")
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("id required: %w", apiframework.ErrBadPathValue), apiframework.DeleteOperation)
		return
	}

	if err := h.service.Delete(ctx, id); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, "deleted") // @response string
}

// Lists all affinity groups in the system.
//
// Returns basic group information without associated backends or models.
func (h *groupHandler) listAffinityGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	groups, err := h.service.ListAll(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, groups) // @response []runtimetypes.AffinityGroup
}

// Lists groups filtered by purpose type with pagination support.
//
// Purpose types categorize groups (e.g., "Internal Embeddings", "Internal Tasks").
// Accepts 'cursor' (RFC3339Nano timestamp) and 'limit' parameters for pagination.
func (h *groupHandler) listAffinityGroupsByPurpose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	purpose := apiframework.GetPathParam(r, "purpose", "The purpose category to filter groups by (e.g., 'embeddings').")
	if purpose == "" {
		apiframework.Error(w, r, fmt.Errorf("id required: %w", apiframework.ErrBadPathValue), apiframework.ListOperation)
		return
	}

	// Parse pagination parameters using the helper
	limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := apiframework.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	if purpose == "" {
		apiframework.Error(w, r, fmt.Errorf("id required: %w", apiframework.ErrBadPathValue), apiframework.ListOperation)
		return
	}

	// Parse pagination parameters from query string
	var cursor *time.Time
	if cursorStr != "" {
		t, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid cursor format, expected RFC3339Nano", apiframework.ErrUnprocessableEntity)
			_ = apiframework.Error(w, r, err, apiframework.ListOperation)
			return
		}
		cursor = &t
	}

	limit := 100 // Default limit
	if limitStr != "" {
		i, err := strconv.Atoi(limitStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid limit format, expected integer", apiframework.ErrUnprocessableEntity)
			_ = apiframework.Error(w, r, err, apiframework.ListOperation)
			return
		}
		limit = i
	}

	groups, err := h.service.ListByPurpose(ctx, purpose, cursor, limit)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, groups) // @response []runtimetypes.AffinityGroup
}

// Associates a backend with an affinity group.
//
// After assignment, the backend can process requests for all models in the affinity group.
// This enables request routing between the backend and models that share this affinity group.
func (h *groupHandler) assignBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := apiframework.GetPathParam(r, "groupID", "The unique identifier of the affinity group.")
	backendID := apiframework.GetPathParam(r, "backendID", "The unique identifier of the backend to be assigned.")

	if groupID == "" || backendID == "" {
		apiframework.Error(w, r, fmt.Errorf("groupID and backendID are required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	if err := h.service.AssignBackend(ctx, groupID, backendID); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, "backend assigned") // @response string
}

// Removes a backend from an affinity group.
//
// After removal, the backend will no longer be eligible to process requests for models in this affinity group.
// Requests requiring models from this affinity group will no longer be routed to this backend.
func (h *groupHandler) removeBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := apiframework.GetPathParam(r, "groupID", "The unique identifier of the affinity group.")
	backendID := apiframework.GetPathParam(r, "backendID", "The unique identifier of the backend to be removed.")

	if groupID == "" || backendID == "" {
		apiframework.Error(w, r, fmt.Errorf("groupID and backendID required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	if err := h.service.RemoveBackend(ctx, groupID, backendID); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, "backend removed") // @response string
}

// Lists all backends associated with a specific affinity group.
//
// Returns basic backend information without runtime state.
func (h *groupHandler) listBackendsByGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := apiframework.GetPathParam(r, "groupID", "The unique identifier of the affinity group.")
	if groupID == "" {
		apiframework.Error(w, r, fmt.Errorf("groupID required: %w", apiframework.ErrBadPathValue), apiframework.ListOperation)
		return
	}

	backends, err := h.service.ListBackends(ctx, groupID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, backends) // @response []runtimetypes.Backend
}

// Lists all affinity groups that a specific backend belongs to.
// Useful for understanding which model sets a backend has access to.
func (h *groupHandler) listAffinityGroupsForBackend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	backendID := apiframework.GetPathParam(r, "backendID", "The unique identifier of the backend.")
	if backendID == "" {
		apiframework.Error(w, r, fmt.Errorf("backendID required: %w", apiframework.ErrBadPathValue), apiframework.ListOperation)
		return
	}

	groups, err := h.service.ListAffinityGroupsForBackend(ctx, backendID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, groups) // @response []runtimetypes.AffinityGroup
}

// Associates a model with an affinity group.
//
// After assignment, requests for this model can be routed to any backend in the affinity group.
// This enables request routing between the model and backends that share this affinity group.
func (h *groupHandler) assignModelToAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := apiframework.GetPathParam(r, "groupID", "The unique identifier of the affinity group.")
	modelID := apiframework.GetPathParam(r, "modelID", "The unique identifier of the model to be assigned.")

	if groupID == "" || modelID == "" {
		apiframework.Error(w, r, fmt.Errorf("groupID and modelID required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	if err := h.service.AssignModel(ctx, groupID, modelID); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, "model assigned") // @response string
}

// Removes a model from an affinity group.
//
// After removal, requests for this model will no longer be routed to backends in this affinity group.
// This model can still be used with backends in other groups where it remains assigned.
func (h *groupHandler) removeModelFromAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := apiframework.GetPathParam(r, "groupID", "The unique identifier of the affinity group.")
	modelID := apiframework.GetPathParam(r, "modelID", "The unique identifier of the model to be removed.")

	if groupID == "" || modelID == "" {
		apiframework.Error(w, r, fmt.Errorf("groupID and modelID required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	if err := h.service.RemoveModel(ctx, groupID, modelID); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, "model removed") // @response string
}

// Lists all models associated with a specific affinity group.
//
// Returns basic model information without backend-specific details.
func (h *groupHandler) listModelsByAffinityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := apiframework.GetPathParam(r, "groupID", "The unique identifier of the affinity group.")
	if groupID == "" {
		apiframework.Error(w, r, fmt.Errorf("groupID required: %w", apiframework.ErrBadPathValue), apiframework.ListOperation)
		return
	}

	models, err := h.service.ListModels(ctx, groupID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, models) // @response []runtimetypes.Model
}

// Lists all affinity groups that a specific model belongs to.
//
// Useful for understanding where a model is deployed across the system.
func (h *groupHandler) listAffinityGroupsForModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := apiframework.GetPathParam(r, "modelID", "The unique identifier of the model.")
	if modelID == "" {
		apiframework.Error(w, r, fmt.Errorf("modelID required: %w", apiframework.ErrBadPathValue), apiframework.ListOperation)
		return
	}

	groups, err := h.service.ListAffinityGroupsForModel(ctx, modelID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, groups) // @response []runtimetypes.AffinityGroup
}
