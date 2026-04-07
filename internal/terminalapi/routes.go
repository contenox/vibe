package terminalapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/terminalservice"
	"golang.org/x/net/websocket"
)

// AddRoutes registers interactive terminal endpoints. If enabled is false, this is a no-op.
func AddRoutes(mux *http.ServeMux, svc terminalservice.Service, auth middleware.AuthZReader, enabled bool) {
	if !enabled {
		return
	}
	h := &handler{svc: svc, auth: auth}
	mux.HandleFunc("GET /terminal/sessions", h.listSessions)
	mux.HandleFunc("POST /terminal/sessions", h.createSession)
	mux.HandleFunc("GET /terminal/sessions/{id}", h.getSession)
	mux.HandleFunc("PATCH /terminal/sessions/{id}", h.patchSession)
	mux.HandleFunc("DELETE /terminal/sessions/{id}", h.deleteSession)
	mux.Handle("GET /terminal/sessions/{id}/ws", h.wsHandler())
}

type handler struct {
	svc  terminalservice.Service
	auth middleware.AuthZReader
}

type createSessionRequest struct {
	CWD   string `json:"cwd"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	Shell string `json:"shell,omitempty"`
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
	req, err := apiframework.Decode[createSessionRequest](r)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	cwd := strings.TrimSpace(req.CWD)
	shell := strings.TrimSpace(req.Shell)

	out, err := h.svc.Create(ctx, principal, terminalservice.CreateRequest{
		CWD:   cwd,
		Cols:  req.Cols,
		Rows:  req.Rows,
		Shell: shell,
	})
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	resp := createSessionResponse{
		ID:     out.ID,
		WSPath: "/terminal/sessions/" + out.ID + "/ws",
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, resp)
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
	_ = apiframework.Encode(w, r, http.StatusOK, items)
}

func (h *handler) getSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.")
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
	_ = apiframework.Encode(w, r, http.StatusOK, sess)
}

func (h *handler) patchSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	principal, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.")
	if id == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("id is required"), apiframework.UpdateOperation)
		return
	}
	body, err := apiframework.Decode[patchSessionRequest](r)
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
	id := apiframework.GetPathParam(r, "id", "The unique identifier of the terminal session.")
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

// wsHandler returns a websocket.Server that upgrades and bridges I/O to the PTY.
// Auth is checked inside the Handshake callback (before upgrade completes).
func (h *handler) wsHandler() http.Handler {
	s := &websocket.Server{
		Handshake: func(cfg *websocket.Config, req *http.Request) error {
			// Accept any origin.
			cfg.Origin, _ = websocket.Origin(cfg, req)
			return nil
		},
	}
	s.Handler = func(ws *websocket.Conn) {
		req := ws.Request()
		principal, err := h.auth.GetIdentity(req.Context())
		if err != nil {
			return
		}
		id := req.PathValue("id")
		if id == "" {
			// PathValue may not be available on the websocket request.
			// Extract from URL path as fallback.
			parts := strings.Split(req.URL.Path, "/")
			for i, p := range parts {
				if p == "sessions" && i+1 < len(parts) && parts[i+1] != "ws" {
					id = parts[i+1]
					break
				}
			}
		}
		if id == "" {
			return
		}

		ws.PayloadType = websocket.BinaryFrame
		resizeCh := make(chan terminalservice.ResizeMsg, 4)
		defer close(resizeCh)

		rw := &termConn{ws: ws, resizeCh: resizeCh}
		if err := h.svc.Attach(context.Background(), principal, id, rw, resizeCh); err != nil {
			slog.Error("terminal attach error", "session", id, "error", err)
		}
	}
	return s
}

// termConn bridges a websocket.Conn to io.ReadWriteCloser for PTY I/O.
// Read intercepts JSON resize messages and forwards only terminal input.
// Write sends binary frames to the client.
type termConn struct {
	ws       *websocket.Conn
	resizeCh chan<- terminalservice.ResizeMsg
	buf      []byte
}

func (c *termConn) Read(p []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	for {
		buf := make([]byte, 32*1024)
		n, err := c.ws.Read(buf)
		if err != nil {
			return 0, err
		}
		data := buf[:n]

		// Check if this is a resize JSON message.
		var msg struct {
			Type string `json:"type"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0 {
			select {
			case c.resizeCh <- terminalservice.ResizeMsg{Cols: msg.Cols, Rows: msg.Rows}:
			default:
			}
			continue
		}

		// Terminal input data.
		copied := copy(p, data)
		if copied < len(data) {
			c.buf = data[copied:]
		}
		return copied, nil
	}
}

func (c *termConn) Write(p []byte) (int, error) { return c.ws.Write(p) }
func (c *termConn) Close() error                { return c.ws.Close() }
