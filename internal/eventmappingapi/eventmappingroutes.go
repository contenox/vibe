package eventmappingapi

import (
	"fmt"
	"net/http"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/eventmappingservice"
	"github.com/contenox/vibe/eventstore"
)

// AddMappingRoutes registers all event mapping API endpoints
func AddMappingRoutes(mux *http.ServeMux, mappingService eventmappingservice.Service) {
	h := &mappingHandler{service: mappingService}

	// Mapping CRUD endpoints
	mux.HandleFunc("POST /mappings", h.createMapping)
	mux.HandleFunc("GET /mappings", h.listMappings)
	// Use query parameter instead of path parameter for paths that may contain slashes
	mux.HandleFunc("GET /mapping", h.getMapping)
	mux.HandleFunc("PUT /mapping", h.updateMapping)
	mux.HandleFunc("DELETE /mapping", h.deleteMapping)
}

type mappingHandler struct {
	service eventmappingservice.Service
}

// Creates a new event mapping configuration.
//
// Mappings define how to extract structured events from incoming webhook payloads.
// They specify how to map JSON fields and headers to event properties like aggregate_id, event_type, etc.
func (h *mappingHandler) createMapping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	config, err := serverops.Decode[eventstore.MappingConfig](r) // @request eventstore.MappingConfig
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	if err := h.service.CreateMapping(ctx, &config); err != nil {
		_ = serverops.Error(w, r, err, serverops.CreateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusCreated, config) // @response eventstore.MappingConfig
}

// Lists all configured event mappings.
//
// Returns mappings sorted by path in ascending order.
func (h *mappingHandler) listMappings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	mappings, err := h.service.ListMappings(ctx)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, mappings) // @response []*eventstore.MappingConfig
}

// Retrieves details for a specific event mapping by path.
func (h *mappingHandler) getMapping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Use query parameter instead of path parameter
	path := serverops.GetQueryParam(r, "path", "", "The unique path identifier for the mapping.")
	if path == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing path query parameter %w", serverops.ErrBadPathValue), serverops.GetOperation)
		return
	}

	config, err := h.service.GetMapping(ctx, path)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.GetOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, config) // @response eventstore.MappingConfig
}

// Updates an existing event mapping configuration.
//
// The path from the query parameter overrides any path in the request body.
func (h *mappingHandler) updateMapping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Use query parameter instead of path parameter
	path := serverops.GetQueryParam(r, "path", "", "The unique path identifier for the mapping.")
	if path == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing path query parameter %w", serverops.ErrBadPathValue), serverops.UpdateOperation)
		return
	}

	config, err := serverops.Decode[eventstore.MappingConfig](r) // @request eventstore.MappingConfig
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	config.Path = path
	if err := h.service.UpdateMapping(ctx, &config); err != nil {
		_ = serverops.Error(w, r, err, serverops.UpdateOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, config) // @response eventstore.MappingConfig
}

// Deletes an event mapping configuration by path.
//
// Returns a simple confirmation message on success.
func (h *mappingHandler) deleteMapping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Use query parameter instead of path parameter
	path := serverops.GetQueryParam(r, "path", "", "The unique path identifier for the mapping.")
	if path == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing path query parameter %w", serverops.ErrBadPathValue), serverops.DeleteOperation)
		return
	}

	if err := h.service.DeleteMapping(ctx, path); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "mapping removed") // @response string
}
