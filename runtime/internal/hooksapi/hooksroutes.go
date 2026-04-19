package hooksapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/runtime/hookproviderservice"
	"github.com/contenox/contenox/runtime/runtimetypes"
)

func AddRemoteHookRoutes(mux *http.ServeMux, service hookproviderservice.Service) {
	s := &remoteHookService{service: service}

	// CRUD for remote hook configurations
	mux.HandleFunc("POST /hooks/remote", s.create)
	mux.HandleFunc("GET /hooks/remote", s.list)
	mux.HandleFunc("GET /hooks/remote/{id}", s.get)
	mux.HandleFunc("GET /hooks/remote/by-name/{name}", s.getByName)
	mux.HandleFunc("PUT /hooks/remote/{id}", s.update)
	mux.HandleFunc("DELETE /hooks/remote/{id}", s.delete)

	// NEW: Local hooks endpoint
	mux.HandleFunc("GET /hooks/local", s.listLocal)

	// Endpoint to get all hook schemas
	mux.HandleFunc("GET /hooks/schemas", s.getSchemas)
}

type remoteHookService struct {
	service hookproviderservice.Service
}

// Lists local hooks supported by the runtime.
//
// Returns a list of locally registered hooks and their tools.
func (s *remoteHookService) listLocal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	localHooks, err := s.service.ListLocalHooks(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, localHooks) // @response []hookproviderservice.LocalHook
}

// Retrieves the OpenAPI schema documents for all supported hook types.
//
// This response is intentionally dynamic: it returns a map keyed by hook name
// whose values are full OpenAPI schema documents for that hook.
func (s *remoteHookService) getSchemas(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	schemas, err := s.service.GetSchemasForSupportedHooks(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, schemas) // @response object
}

// Creates a new remote hook configuration.
//
// Remote hooks allow task-chains to trigger external HTTP services during execution.
func (s *remoteHookService) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	hook, err := apiframework.Decode[runtimetypes.RemoteHook](r) // @request runtimetypes.RemoteHook
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	if err := s.service.Create(ctx, &hook); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusCreated, hook) // @response runtimetypes.RemoteHook
}

// Lists remote hooks, optionally filtering by a unique name.
//
// Returns a list of remote hooks.
func (s *remoteHookService) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := apiframework.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	var cursor *time.Time
	if cursorStr != "" {
		parsedTime, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			_ = apiframework.Error(w, r, fmt.Errorf("invalid cursor format: %w", err), apiframework.ListOperation)
			return
		}
		cursor = &parsedTime
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		_ = apiframework.Error(w, r, fmt.Errorf("invalid limit format: %w", err), apiframework.ListOperation)
		return
	}

	hooks, err := s.service.List(ctx, cursor, limit)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, hooks) // @response []runtimetypes.RemoteHook
}

// Retrieves a specific remote hook configuration by ID.
//
// Returns a simple "deleted" confirmation message on success.
func (s *remoteHookService) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the remote hook.")

	hook, err := s.service.Get(ctx, id)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, hook) // @response runtimetypes.RemoteHook
}

// Updates an existing remote hook configuration.
//
// The ID from the URL path overrides any ID in the request body.
func (s *remoteHookService) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the remote hook.")

	hook, err := apiframework.Decode[runtimetypes.RemoteHook](r) // @request runtimetypes.RemoteHook
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	hook.ID = id
	if err := s.service.Update(ctx, &hook); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, hook) // @response runtimetypes.RemoteHook
}

// Deletes a remote hook configuration by ID.
//
// Returns a simple "deleted" confirmation message on success.
func (s *remoteHookService) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique identifier for the remote hook.")

	if err := s.service.Delete(ctx, id); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, "deleted") // @response string
}

// Retrieves a remote hook configuration by name.
//
// Returns a simple "deleted" confirmation message on success.
func (s *remoteHookService) getByName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := apiframework.GetPathParam(r, "name", "The unique name for the remote hook.")
	hook, err := s.service.GetByName(ctx, name)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, hook) // @response runtimetypes.RemoteHook
}
