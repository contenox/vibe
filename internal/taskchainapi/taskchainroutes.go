package taskchainapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/taskchainservice"
	"github.com/contenox/vibe/taskengine"
)

func AddTaskChainRoutes(mux *http.ServeMux, service taskchainservice.Service) {
	h := &handler{service: service}
	mux.HandleFunc("POST /taskchains", h.createTaskChain)
	mux.HandleFunc("GET /taskchains", h.listTaskChains)
	mux.HandleFunc("GET /taskchains/{id}", h.getTaskChain)
	mux.HandleFunc("PUT /taskchains/{id}", h.updateTaskChain)
	mux.HandleFunc("DELETE /taskchains/{id}", h.deleteTaskChain)
}

type handler struct {
	service taskchainservice.Service
}

// Creates a new task chain definition.
//
// The task chain is stored in the system's KV store for later execution.
// Task chains define workflows with conditional branches, external hooks, and captured execution state.
func (h *handler) createTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chain, err := apiframework.Decode[taskengine.TaskChainDefinition](r) // @request taskengine.TaskChainDefinition
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	if err := h.service.Create(ctx, &chain); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusCreated, chain) // @response taskengine.TaskChainDefinition
}

// Retrieves a specific task chain by ID.
func (h *handler) getTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the task chain.")
	if id == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("task chain ID is required: %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}

	chain, err := h.service.Get(ctx, id)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, chain) // @response taskengine.TaskChainDefinition
}

// Updates an existing task chain definition.
func (h *handler) updateTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the task chain.")
	if id == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("task chain ID is required: %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}

	chain, err := apiframework.Decode[taskengine.TaskChainDefinition](r) // @request taskengine.TaskChainDefinition
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	// Ensure the ID in the URL matches the chain data
	if chain.ID != "" && chain.ID != id {
		err = fmt.Errorf("%w: ID in payload does not match URL", apiframework.ErrUnprocessableEntity)
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	chain.ID = id // enforce ID from URL
	if err := h.service.Update(ctx, &chain); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, chain) // @response taskengine.TaskChainDefinition
}

// Lists all task chain definitions with pagination.
func (h *handler) listTaskChains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Parse pagination parameters
	limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := apiframework.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

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

	limit := 100
	if limitStr != "" {
		i, err := strconv.Atoi(limitStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid limit format, expected integer", apiframework.ErrUnprocessableEntity)
			_ = apiframework.Error(w, r, err, apiframework.ListOperation)
			return
		}
		if i < 1 {
			err = fmt.Errorf("%w: limit must be positive", apiframework.ErrUnprocessableEntity)
			_ = apiframework.Error(w, r, err, apiframework.ListOperation)
			return
		}
		limit = i
	}

	chains, err := h.service.List(ctx, cursor, limit)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, chains) // @response []*taskengine.TaskChainDefinition
}

// Deletes a task chain definition.
func (h *handler) deleteTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the task chain to delete.")
	if id == "" {
		apiframework.Error(w, r, fmt.Errorf("task chain ID is required: %w", apiframework.ErrBadPathValue), apiframework.DeleteOperation)
		return
	}

	if err := h.service.Delete(ctx, id); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, fmt.Sprintf("task chain %s deleted", id)) // @response string
}
