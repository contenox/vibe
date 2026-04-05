package taskchainapi

import (
	"fmt"
	"net/http"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
)

// AddTaskChainRoutes registers CRUD for task chains stored as VFS JSON files.
// All operations require query parameter "path" (relative VFS path, e.g. chain-explore.json).
// Listing is not provided — use GET /files. GET by logical id is not supported at HTTP layer;
// use path or call the in-process Service.Get(ref) which resolves logical ids.
func AddTaskChainRoutes(mux *http.ServeMux, service taskchainservice.Service) {
	h := &handler{service: service}
	mux.HandleFunc("GET /taskchains", h.getTaskChain)
	mux.HandleFunc("POST /taskchains", h.createTaskChain)
	mux.HandleFunc("PUT /taskchains", h.updateTaskChain)
	mux.HandleFunc("DELETE /taskchains", h.deleteTaskChain)
}

type handler struct {
	service taskchainservice.Service
}

func (h *handler) pathFromQuery(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", fmt.Errorf("%w: query parameter path is required", apiframework.ErrBadRequest)
	}
	return taskchainservice.NormalizeVFSPath(p)
}

// Retrieves a task chain JSON document at the given VFS path.
func (h *handler) getTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path, err := h.pathFromQuery(r)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	chain, err := h.service.Get(ctx, path)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, chain) // @response taskengine.TaskChainDefinition
}

// Creates a new task chain file at path (must not exist).
func (h *handler) createTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path, err := h.pathFromQuery(r)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	chain, err := apiframework.Decode[taskengine.TaskChainDefinition](r) // @request taskengine.TaskChainDefinition
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	if err := h.service.CreateAtPath(ctx, path, &chain); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, chain) // @response taskengine.TaskChainDefinition
}

// Updates the task chain file at path.
func (h *handler) updateTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path, err := h.pathFromQuery(r)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	chain, err := apiframework.Decode[taskengine.TaskChainDefinition](r) // @request taskengine.TaskChainDefinition
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	if err := h.service.UpdateAtPath(ctx, path, &chain); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, chain) // @response taskengine.TaskChainDefinition
}

// Deletes the task chain file at path.
func (h *handler) deleteTaskChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path, err := h.pathFromQuery(r)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	if err := h.service.DeleteByPath(ctx, path); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, fmt.Sprintf("task chain file %s deleted", path)) // @response string
}
