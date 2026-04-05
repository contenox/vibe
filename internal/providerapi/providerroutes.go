package providerapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/internal/runtimestate"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/providerservice"
)

func AddProviderRoutes(mux *http.ServeMux, providerService providerservice.Service) {
	p := &providerManager{providerService: providerService}

	mux.HandleFunc("POST /providers/ollama/configure", p.configure("ollama"))
	mux.HandleFunc("POST /providers/openai/configure", p.configure("openai"))
	mux.HandleFunc("POST /providers/gemini/configure", p.configure("gemini"))
	mux.HandleFunc("GET /providers/ollama/status", p.status("ollama"))
	mux.HandleFunc("GET /providers/openai/status", p.status("openai"))
	mux.HandleFunc("GET /providers/gemini/status", p.status("gemini"))
	mux.HandleFunc("DELETE /providers/{providerType}/config", p.deleteConfig)
	mux.HandleFunc("GET /providers/configs", p.listConfigs)
	mux.HandleFunc("GET /providers/{providerType}/config", p.get)

}

type providerManager struct {
	providerService providerservice.Service
}

type ConfigureRequest struct {
	APIKey string `json:"apiKey"`
	Upsert bool   `json:"upsert"`
}

type StatusResponse struct {
	Configured bool      `json:"configured"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Provider   string    `json:"provider" example:"ollama"`
}

// Configures authentication for an external provider (Ollama Cloud, OpenAI, or Gemini).
//
// Requires a valid API key for the specified provider type.
// The 'upsert' parameter determines whether to update existing configuration.
// After suggesful configuration the system will provision a virtual backend for that provider.
// Also all available models from that given provider will be declared by the system.
// This provider can then be added to one or many groups to allow usage of the models.
func (p *providerManager) configure(providerType string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := apiframework.Decode[ConfigureRequest](r) // @request providerapi.ConfigureRequest
		if err != nil {
			_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
			return
		}

		if req.APIKey == "" {
			_ = apiframework.Error(w, r, fmt.Errorf("api key is required"), apiframework.CreateOperation)
			return
		}

		cfg := &runtimestate.ProviderConfig{
			APIKey: req.APIKey,
			Type:   providerType,
		}

		if err := p.providerService.SetProviderConfig(r.Context(), providerType, req.Upsert, cfg); err != nil {
			_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
			return
		}
		_ = apiframework.Encode(w, r, http.StatusOK, StatusResponse{
			Configured: true,
			Provider:   providerType,
		}) // @response providerapi.StatusResponse
	}
}

// Checks configuration status for an external provider.
//
// Returns whether the provider is properly configured with valid credentials.
func (p *providerManager) status(providerType string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := p.providerService.GetProviderConfig(r.Context(), providerType)
		if errors.Is(err, libdb.ErrNotFound) {
			_ = apiframework.Encode(w, r, http.StatusOK, StatusResponse{
				Configured: false,
				Provider:   providerType,
			}) // @response providerapi.StatusResponse
			return
		}
		if err != nil {
			_ = apiframework.Error(w, r, err, apiframework.GetOperation)
			return
		}
		_ = apiframework.Encode(w, r, http.StatusOK, StatusResponse{
			Configured: true,
			Provider:   providerType,
		}) // @response providerapi.StatusResponse
	}
}

// Removes provider configuration from the system.
//
// After deletion, the provider will no longer be available for model execution.
func (p *providerManager) deleteConfig(w http.ResponseWriter, r *http.Request) {
	providerType := apiframework.GetPathParam(r, "providerType", "The type of the provider to delete (e.g., 'ollama', 'openai', 'gemini').")
	if providerType == "" {
		_ = apiframework.Error(w, r, errors.New("providerType is required in path"), apiframework.DeleteOperation)
		return
	}

	if err := p.providerService.DeleteProviderConfig(r.Context(), providerType); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.DeleteOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, "Provider config deleted successfully") // @response string
}

// Lists all configured external providers with pagination support.
func (p *providerManager) listConfigs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Use the new helper for pagination parameters
	limitStr := apiframework.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := apiframework.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	// Parse pagination parameters from the retrieved strings
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

	configs, err := p.providerService.ListProviderConfigs(ctx, cursor, limit)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, configs) // @response []runtimestate.ProviderConfig
}

// Retrieves configuration details for a specific external provider.
func (p *providerManager) get(w http.ResponseWriter, r *http.Request) {
	providerType := apiframework.GetPathParam(r, "providerType", "The type of the provider to retrieve (e.g., 'ollama', 'openai', 'gemini').")
	if providerType == "" {
		_ = apiframework.Error(w, r, errors.New("providerType is required in path"), apiframework.GetOperation)
		return
	}

	config, err := p.providerService.GetProviderConfig(r.Context(), providerType)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, config) // @response runtimestate.ProviderConfig
}
