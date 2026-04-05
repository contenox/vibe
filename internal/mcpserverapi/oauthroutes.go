package mcpserverapi

import (
	"net/http"
	"strings"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/mcpserverservice"
)

type oauthStartRequest struct {
	RedirectBase string `json:"redirectBase"`
}

type oauthStartResponse struct {
	AuthorizationURL string `json:"authorizationUrl"`
}

func (h *mcpServerHandler) oauthStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	id := apiframework.GetPathParam(r, "id", "The unique ID of the MCP server.")
	req, err := apiframework.Decode[oauthStartRequest](r) // @request mcpserverapi.oauthStartRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	res, err := h.svc.StartOAuth(ctx, id, req.RedirectBase)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	resp := oauthStartResponse{
		AuthorizationURL: res.AuthorizationURL,
	}
	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response mcpserverapi.oauthStartResponse
}

func (h *mcpServerHandler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := mcpserverservice.OAuthCallbackRequest{
		State:            strings.TrimSpace(apiframework.GetQueryParam(r, "state", "", "The OAuth state value returned by the provider.")),
		Code:             strings.TrimSpace(apiframework.GetQueryParam(r, "code", "", "The OAuth authorization code returned by the provider.")),
		Error:            strings.TrimSpace(apiframework.GetQueryParam(r, "error", "", "The provider error code, when authorization fails.")),
		ErrorDescription: strings.TrimSpace(apiframework.GetQueryParam(r, "error_description", "", "The provider error description, when authorization fails.")),
	}

	res, err := h.svc.CompleteOAuth(ctx, req)
	if err != nil {
		http.Redirect(w, r, oauthRedirectURL(res, "error", err.Error()), http.StatusFound)
		return
	}

	srv, getErr := h.svc.GetByName(ctx, res.ServerName)
	if getErr == nil {
		h.publishDeleted(ctx, srv.Name)
		h.publishCreated(ctx, srv)
	}
	http.Redirect(w, r, oauthRedirectURL(res, "success", ""), http.StatusFound)
}
