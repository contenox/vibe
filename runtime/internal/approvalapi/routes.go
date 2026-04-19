// Package approvalapi provides the HTTP endpoint for resolving HITL approval
// requests emitted by localhooks.HITLWrapper.
package approvalapi

import (
	"net/http"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/runtime/hitlservice"
)

// AddRoutes registers POST /approvals/{approvalId} on mux.
func AddRoutes(mux *http.ServeMux, svc hitlservice.Service, auth middleware.AuthZReader) {
	h := &handler{svc: svc, auth: auth}
	mux.HandleFunc("POST /approvals/{approvalId}", h.respond)
}

type handler struct {
	svc  hitlservice.Service
	auth middleware.AuthZReader
}

// respondBody is the request body for POST /api/approvals/{approvalId}.
type respondBody struct {
	Approved bool `json:"approved"`
}

// respond approves or denies a pending HITL tool-call gate.
// Returns 204 on success, 404 if the approvalId is not found (already resolved or timed out).
func (h *handler) respond(w http.ResponseWriter, r *http.Request) {
	if _, err := h.auth.GetIdentity(r.Context()); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	approvalID := apiframework.GetPathParam(r, "approvalId", "The UUID of the pending HITL approval request.")
	if approvalID == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("approvalId is required"), apiframework.CreateOperation)
		return
	}

	body, err := apiframework.Decode[respondBody](r) // @request approvalapi.respondBody
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	if !h.svc.Respond(approvalID, body.Approved) {
		_ = apiframework.Error(w, r, apiframework.NotFound("approval not found or already resolved"), apiframework.CreateOperation)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
