package terminalapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/terminalservice"
	"github.com/contenox/contenox/workspaceservice"
)

// AddRoutes registers interactive terminal endpoints. If enabled is false, this is a no-op.
// When workspacesConfigured is true, ws must be non-nil; create accepts workspaceId or cwd (exactly one).
func AddRoutes(mux *http.ServeMux, svc terminalservice.Service, auth middleware.AuthZReader, enabled bool, ws workspaceservice.Service, workspacesConfigured bool) {
	if !enabled {
		return
	}
	h := &handler{svc: svc, auth: auth, ws: ws, workspacesOn: workspacesConfigured}
	mux.HandleFunc("GET /terminal/sessions", h.listSessions)
	mux.HandleFunc("POST /terminal/sessions", h.createSession)
	mux.HandleFunc("GET /terminal/sessions/{id}", h.getSession)
	mux.HandleFunc("PATCH /terminal/sessions/{id}", h.patchSession)
	mux.HandleFunc("DELETE /terminal/sessions/{id}", h.deleteSession)
	mux.HandleFunc("GET /terminal/sessions/{id}/ws", h.webSocket)
}

type handler struct {
	svc            terminalservice.Service
	auth           middleware.AuthZReader
	ws             workspaceservice.Service
	workspacesOn   bool
}

type createSessionRequest struct {
	WorkspaceID string `json:"workspaceId"`
	CWD         string `json:"cwd"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
	Shell       string `json:"shell,omitempty"`
}

type createSessionResponse struct {
	ID     string `json:"id"`
	WSPath string `json:"wsPath"`
}

type patchSessionRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Creates a PTY-backed shell session. Connect with WebSocket to wsPath.
func (h *handler) createSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	req, err := apiframework.Decode[createSessionRequest](r) // @request terminalapi.createSessionRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	cwd := strings.TrimSpace(req.CWD)
	wsID := strings.TrimSpace(req.WorkspaceID)
	if wsID != "" && cwd != "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("set exactly one of workspaceId or cwd"), apiframework.CreateOperation)
		return
	}
	if wsID == "" && cwd == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("workspaceId or cwd is required"), apiframework.CreateOperation)
		return
	}

	var workspaceIDPersist string
	shell := strings.TrimSpace(req.Shell)
	if wsID != "" {
		if !h.workspacesOn || h.ws == nil {
			_ = apiframework.Error(w, r, apiframework.BadRequest("workspaces are not configured on this server"), apiframework.CreateOperation)
			return
		}
		dto, err := h.ws.Get(ctx, principal, wsID)
		if err != nil {
			if errors.Is(err, workspaceservice.ErrNotFound) {
				_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.CreateOperation)
				return
			}
			_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
			return
		}
		cwd = dto.Path
		workspaceIDPersist = wsID
		if shell == "" {
			shell = strings.TrimSpace(dto.Shell)
		}
	}

	out, err := h.svc.Create(ctx, principal, terminalservice.CreateRequest{
		CWD:         cwd,
		WorkspaceID: workspaceIDPersist,
		Cols:        req.Cols,
		Rows:        req.Rows,
		Shell:       shell,
	})
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	resp := createSessionResponse{
		ID:     out.ID,
		WSPath: "/terminal/sessions/" + out.ID + "/ws",
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, resp) // @response terminalapi.createSessionResponse
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
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
	_ = apiframework.Encode(w, r, http.StatusOK, items) // @response []terminalstore.Session
}

func (h *handler) getSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.GetOperation)
		return
	}
	sess, err := h.svc.Get(ctx, principal, id)
	if err != nil {
		if errors.Is(err, terminalservice.ErrSessionNotFound) {
			_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.GetOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, sess) // @response terminalstore.Session
}

func (h *handler) patchSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.UpdateOperation)
		return
	}
	body, err := apiframework.Decode[patchSessionRequest](r) // @request terminalapi.patchSessionRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	if body.Cols <= 0 || body.Rows <= 0 {
		_ = apiframework.Error(w, r, apiframework.BadRequest("cols and rows must be positive"), apiframework.UpdateOperation)
		return
	}
	err = h.svc.UpdateGeometry(ctx, principal, id, body.Cols, body.Rows)
	if err != nil {
		if errors.Is(err, terminalservice.ErrSessionNotFound) {
			_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.UpdateOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	principal, err := h.auth.GetIdentity(r.Context())
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.DeleteOperation)
		return
	}
	err = h.svc.Close(r.Context(), principal, id)
	if err != nil {
		if errors.Is(err, terminalservice.ErrSessionNotFound) {
			_ = apiframework.Error(w, r, apiframework.ErrNotFound, apiframework.DeleteOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) webSocket(w http.ResponseWriter, r *http.Request) {
	principal, err := h.auth.GetIdentity(r.Context())
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.") // @param id string
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.GetOperation)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       nil,
		InsecureSkipVerify: true,
	})
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	defer c.Close(websocket.StatusInternalError, "")

	err = h.svc.Attach(r.Context(), principal, id, c)
	if err != nil {
		if errors.Is(err, terminalservice.ErrSessionNotFound) {
			_ = c.Close(websocket.StatusPolicyViolation, "session not found")
			return
		}
		if errors.Is(err, terminalservice.ErrNotImplemented) {
			_ = c.Close(websocket.StatusPolicyViolation, "not implemented")
			return
		}
		_ = c.Close(websocket.StatusInternalError, err.Error())
	}
}
