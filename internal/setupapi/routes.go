// Package setupapi exposes REST routes for Beam onboarding (readiness + CLI defaults in KV).
package setupapi

import (
	"net/http"
	"strings"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/stateservice"
)

// AddSetupRoutes registers GET /setup-status and PUT /cli-config on mux.
func AddSetupRoutes(mux *http.ServeMux, stateService stateservice.Service, auth middleware.AuthZReader) {
	h := &setupHandler{state: stateService, auth: auth}
	mux.HandleFunc("GET /setup-status", h.getStatus)
	mux.HandleFunc("PUT /cli-config", h.putCLIConfig)
}

type setupHandler struct {
	state stateservice.Service
	auth  middleware.AuthZReader
}

// Returns runtime readiness: CLI defaults, backend counts, and actionable issues for Beam.
func (h *setupHandler) getStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	res, err := h.state.SetupStatus(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}
	_ = apiframework.Encode(w, r, http.StatusOK, res) // @response setupcheck.Result
}

type putCLIConfigRequest struct {
	DefaultModel    string `json:"default-model"`
	DefaultProvider string `json:"default-provider"`
	DefaultChain    string `json:"default-chain"`
	HITLPolicyName  string `json:"hitl-policy-name"`
}

type putCLIConfigResponse struct {
	DefaultModel    string `json:"defaultModel"`
	DefaultProvider string `json:"defaultProvider"`
	DefaultChain    string `json:"defaultChain"`
	HITLPolicyName  string `json:"hitlPolicyName"`
}

// Updates CLI default keys (model, provider, chain, hitl-policy-name) in KV (same as contenox config set).
func (h *setupHandler) putCLIConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.auth.GetIdentity(ctx); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}
	body, err := apiframework.Decode[putCLIConfigRequest](r) // @request setupapi.putCLIConfigRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	if strings.TrimSpace(body.DefaultModel) == "" &&
		strings.TrimSpace(body.DefaultProvider) == "" &&
		strings.TrimSpace(body.DefaultChain) == "" &&
		strings.TrimSpace(body.HITLPolicyName) == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("Provide at least one of default-model, default-provider, default-chain, or hitl-policy-name."), apiframework.UpdateOperation)
		return
	}
	snap, err := h.state.SetCLIConfig(ctx, stateservice.CLIConfigPatch{
		DefaultModel:    body.DefaultModel,
		DefaultProvider: body.DefaultProvider,
		DefaultChain:    body.DefaultChain,
		HITLPolicyName:  body.HITLPolicyName,
	})
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.UpdateOperation)
		return
	}
	resp := putCLIConfigResponse{
		DefaultModel:    snap.DefaultModel,
		DefaultProvider: snap.DefaultProvider,
		DefaultChain:    snap.DefaultChain,
		HITLPolicyName:  snap.HITLPolicyName,
	}
	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response setupapi.putCLIConfigResponse
}
