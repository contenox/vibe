// Package mcpserverapi exposes REST endpoints for managing MCP server configurations.
package mcpserverapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/mcpserverservice"
	"github.com/contenox/contenox/mcpworker"
	"github.com/contenox/contenox/runtimetypes"
)

// AddMCPServerRoutes registers the MCP server CRUD routes on the given mux.
// messenger is used to broadcast lifecycle events (created/deleted) so that
// all nodes in the cluster can start or stop their session workers.
func AddMCPServerRoutes(mux *http.ServeMux, svc mcpserverservice.Service, messenger libbus.Messenger, auth middleware.AuthZReader) {
	h := &mcpServerHandler{svc: svc, messenger: messenger, auth: auth}

	mux.HandleFunc("POST /mcp-servers", h.create)
	mux.HandleFunc("GET /mcp-servers", h.list)
	mux.HandleFunc("GET /mcp-servers/by-name/{name}", h.getByName)
	mux.HandleFunc("GET /mcp-servers/{id}", h.get)
	mux.HandleFunc("PUT /mcp-servers/{id}", h.update)
	mux.HandleFunc("DELETE /mcp-servers/{id}", h.delete)
	mux.HandleFunc("POST /mcp-servers/{id}/oauth/start", h.oauthStart)
	mux.HandleFunc("GET /mcp/oauth/callback", h.oauthCallback)
}

type mcpServerHandler struct {
	svc       mcpserverservice.Service
	messenger libbus.Messenger
	auth      middleware.AuthZReader
}

// Creates a new MCP server configuration.
//
// MCP servers allow task-chains to call tools on external Model Context Protocol servers.
// Supported transports: stdio (command + args), sse (url), http (url).
func (h *mcpServerHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srv, err := apiframework.Decode[runtimetypes.MCPServer](r) // @request runtimetypes.MCPServer
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	if err := h.svc.Create(ctx, &srv); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	h.publishCreated(ctx, &srv)
	_ = apiframework.Encode(w, r, http.StatusCreated, srv) // @response runtimetypes.MCPServer
}

// Lists all MCP server configurations.
//
// Returns a paginated list of MCP server configurations.
func (h *mcpServerHandler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	limitStr := apiframework.GetQueryParam(r, "limit", "100", "Maximum number of items to return.")
	cursorStr := apiframework.GetQueryParam(r, "cursor", "", "RFC3339Nano timestamp for pagination cursor.")

	var cursor *time.Time
	if cursorStr != "" {
		t, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			_ = apiframework.Error(w, r, fmt.Errorf("invalid cursor: %w", err), apiframework.ListOperation)
			return
		}
		cursor = &t
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		_ = apiframework.Error(w, r, fmt.Errorf("invalid limit: %w", err), apiframework.ListOperation)
		return
	}

	items, err := h.svc.List(ctx, cursor, limit)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, items) // @response []*runtimetypes.MCPServer
}

// Retrieves an MCP server configuration by its unique ID.
//
// Returns the MCP server configuration.
func (h *mcpServerHandler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique ID of the MCP server.")
	srv, err := h.svc.Get(ctx, id)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, srv) // @response runtimetypes.MCPServer
}

// Retrieves an MCP server configuration by its unique name.
//
// Returns the MCP server configuration.
func (h *mcpServerHandler) getByName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := apiframework.GetPathParam(r, "name", "The unique name of the MCP server.")
	srv, err := h.svc.GetByName(ctx, name)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, srv) // @response runtimetypes.MCPServer
}

// Updates an existing MCP server configuration.
//
// The ID in the URL path takes precedence over any ID in the request body.
// All nodes restart their session worker for this server to pick up the new config.
func (h *mcpServerHandler) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique ID of the MCP server.")
	srv, err := apiframework.Decode[runtimetypes.MCPServer](r) // @request runtimetypes.MCPServer
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	srv.ID = id
	if err := h.svc.Update(ctx, &srv); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	// Delete old worker (by name), then start a fresh one with updated config.
	h.publishDeleted(ctx, srv.Name)
	h.publishCreated(ctx, &srv)
	_ = apiframework.Encode(w, r, http.StatusOK, srv) // @response runtimetypes.MCPServer
}

// Deletes an MCP server configuration by its unique ID.
//
// Returns "deleted" on success. All nodes stop their session worker for this server.
func (h *mcpServerHandler) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := apiframework.GetPathParam(r, "id", "The unique ID of the MCP server.")

	// Fetch name before deletion so we can broadcast it.
	srv, err := h.svc.Get(ctx, id)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	if err := h.svc.Delete(ctx, id); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}
	h.publishDeleted(ctx, srv.Name)
	_ = apiframework.Encode(w, r, http.StatusOK, "deleted") // @response string
}

// ── lifecycle event helpers ────────────────────────────────────────────────────

func (h *mcpServerHandler) publishCreated(ctx context.Context, srv *runtimetypes.MCPServer) {
	data, err := json.Marshal(srv)
	if err != nil {
		slog.Warn("mcpserverapi: failed to marshal created event", "err", err)
		return
	}
	if err := h.messenger.Publish(ctx, mcpworker.SubjectCreated, data); err != nil {
		slog.Warn("mcpserverapi: failed to publish created event", "name", srv.Name, "err", err)
	}
}

func (h *mcpServerHandler) publishDeleted(ctx context.Context, name string) {
	data, err := json.Marshal(mcpworker.MCPDeletedEvent{Name: name})
	if err != nil {
		slog.Warn("mcpserverapi: failed to marshal deleted event", "err", err)
		return
	}
	if err := h.messenger.Publish(ctx, mcpworker.SubjectDeleted, data); err != nil {
		slog.Warn("mcpserverapi: failed to publish deleted event", "name", name, "err", err)
	}
}
