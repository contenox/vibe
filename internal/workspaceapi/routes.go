package workspaceapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/workspaceservice"
)

// AddRoutes registers workspace CRUD (principal-scoped).
func AddRoutes(mux *http.ServeMux, svc workspaceservice.Service, auth middleware.AuthZReader) {
	h := &handler{svc: svc, auth: auth}
	mux.HandleFunc("GET /workspaces", h.list)
	mux.HandleFunc("POST /workspaces", h.create)
	mux.HandleFunc("GET /workspaces/{id}", h.get)
	mux.HandleFunc("PATCH /workspaces/{id}", h.patch)
	mux.HandleFunc("DELETE /workspaces/{id}", h.delete)
}

type handler struct {
	svc  workspaceservice.Service
	auth middleware.AuthZReader
}

type createWorkspaceRequest struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Shell string `json:"shell,omitempty"`
}

type patchWorkspaceRequest struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Shell string `json:"shell,omitempty"`
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := h.auth.GetIdentity(r.Context())
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	body, err := apiframework.Decode[createWorkspaceRequest](r) // @request workspaceapi.createWorkspaceRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	out, err := h.svc.Create(r.Context(), principal, workspaceservice.CreateInput{
		Name: body.Name, Path: body.Path, Shell: body.Shell,
	})
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, out) // @response workspaceservice.WorkspaceDTO
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
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
	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		err = fmt.Errorf("%w: invalid limit format, expected integer", apiframework.ErrUnprocessableEntity)
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	if limit < 1 {
		err = fmt.Errorf("%w: limit must be positive", apiframework.ErrUnprocessableEntity)
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	items, err := h.svc.List(ctx, principal, cursor, limit)
	if err != nil {
		if errors.Is(err, runtimetypes.ErrLimitParamExceeded) {
			_ = apiframework.Error(w, r, err, apiframework.ListOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, items) // @response []workspaceservice.WorkspaceDTO
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the workspace.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.GetOperation)
		return
	}
	out, err := h.svc.Get(ctx, principal, id)
	if err != nil {
		if errors.Is(err, workspaceservice.ErrNotFound) {
			_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.GetOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, out) // @response workspaceservice.WorkspaceDTO
}

func (h *handler) patch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the workspace.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.UpdateOperation)
		return
	}
	body, err := apiframework.Decode[patchWorkspaceRequest](r) // @request workspaceapi.patchWorkspaceRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	out, err := h.svc.Update(ctx, principal, id, workspaceservice.UpdateInput{
		Name: body.Name, Path: body.Path, Shell: body.Shell,
	})
	if err != nil {
		if errors.Is(err, workspaceservice.ErrNotFound) {
			_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.UpdateOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, out) // @response workspaceservice.WorkspaceDTO
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	principal, err := h.auth.GetIdentity(r.Context())
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the workspace.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.DeleteOperation)
		return
	}
	err = h.svc.Delete(r.Context(), principal, id)
	if err != nil {
		if errors.Is(err, workspaceservice.ErrNotFound) {
			_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.DeleteOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
