package modelregistryapi

import (
	"fmt"
	"net/http"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/modelregistry"
	"github.com/contenox/contenox/modelregistryservice"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/google/uuid"
)

func AddRoutes(mux *http.ServeMux, svc modelregistryservice.Service, reg modelregistry.Registry) {
	h := &handler{svc: svc, reg: reg}
	mux.HandleFunc("POST /model-registry", h.create)
	mux.HandleFunc("GET /model-registry", h.list)
	mux.HandleFunc("GET /model-registry/{id}", h.get)
	mux.HandleFunc("PUT /model-registry/{id}", h.update)
	mux.HandleFunc("DELETE /model-registry/{id}", h.delete)
}

type handler struct {
	svc modelregistryservice.Service
	reg modelregistry.Registry
}

// Creates a new user-defined model registry entry.
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, err := apiframework.Decode[runtimetypes.ModelRegistryEntry](r) // @request runtimetypes.ModelRegistryEntry
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	e.ID = uuid.NewString()
	if err := h.svc.Create(ctx, &e); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, e) // @response runtimetypes.ModelRegistryEntry
}

// Lists all known model registry entries (curated + user-added).
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entries, err := h.reg.List(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, entries) // @response []modelregistry.ModelDescriptor
}

// Retrieves a specific model registry entry by ID.
func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the model registry entry.")
	if id == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("missing id parameter %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}
	e, err := h.svc.Get(ctx, id)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, e) // @response runtimetypes.ModelRegistryEntry
}

// Updates an existing user-defined model registry entry.
func (h *handler) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the model registry entry.")
	if id == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("missing id parameter %w", apiframework.ErrBadPathValue), apiframework.UpdateOperation)
		return
	}
	e, err := apiframework.Decode[runtimetypes.ModelRegistryEntry](r) // @request runtimetypes.ModelRegistryEntry
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	e.ID = id
	if err := h.svc.Update(ctx, &e); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, e) // @response runtimetypes.ModelRegistryEntry
}

// Removes a user-defined model registry entry.
func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the model registry entry.")
	if id == "" {
		_ = apiframework.Error(w, r, fmt.Errorf("missing id parameter %w", apiframework.ErrBadPathValue), apiframework.DeleteOperation)
		return
	}
	if err := h.svc.Delete(ctx, id); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, "model registry entry removed") // @response string
}
