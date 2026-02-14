package providerapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/internal/runtimestate"
	libdb "github.com/contenox/vibe/libdbexec"
	"github.com/contenox/vibe/providerservice"
)

func AddProviderRoutes(mux *http.ServeMux, providerService providerservice.Service) {
	p := &providerManager{providerService: providerService}

	mux.HandleFunc("POST /providers/openai/configure", p.configure("openai"))
	mux.HandleFunc("POST /providers/gemini/configure", p.configure("gemini"))
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
	Provider   string    `json:"provider" example:"gemini"`
}

// Configures authentication for an external provider (OpenAI or Gemini).
//
// Requires a valid API key for the specified provider type.
// The 'upsert' parameter determines whether to update existing configuration.
// After suggesful configuration the system will provision a virtual backend for that provider.
// Also all available models from that given provider will be declared by the system.
// This provider can then be added to one or many groups to allow usage of the models.
func (p *providerManager) configure(providerType string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := serverops.Decode[ConfigureRequest](r) // @request providerapi.ConfigureRequest
		if err != nil {
			_ = serverops.Error(w, r, err, serverops.CreateOperation)
			return
		}

		if req.APIKey == "" {
			_ = serverops.Error(w, r, fmt.Errorf("api key is required"), serverops.CreateOperation)
			return
		}

		cfg := &runtimestate.ProviderConfig{
			APIKey: req.APIKey,
			Type:   providerType,
		}

		if err := p.providerService.SetProviderConfig(r.Context(), providerType, req.Upsert, cfg); err != nil {
			_ = serverops.Error(w, r, err, serverops.CreateOperation)
			return
		}
		_ = serverops.Encode(w, r, http.StatusOK, StatusResponse{ // @response providerapi.StatusResponse
			Configured: true,
			Provider:   providerType,
		})
	}
}

// Checks configuration status for an external provider.
//
// Returns whether the provider is properly configured with valid credentials.
func (p *providerManager) status(providerType string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := p.providerService.GetProviderConfig(r.Context(), providerType)
		if errors.Is(err, libdb.ErrNotFound) {
			_ = serverops.Encode(w, r, http.StatusOK, StatusResponse{
				Configured: false,
				Provider:   providerType,
			})
			return
		}
		if err != nil {
			_ = serverops.Error(w, r, err, serverops.GetOperation)
			return
		}
		_ = serverops.Encode(w, r, http.StatusOK, StatusResponse{ // @response providerapi.StatusResponse
			Configured: true,
			Provider:   providerType,
		})
	}
}

// Removes provider configuration from the system.
//
// After deletion, the provider will no longer be available for model execution.
func (p *providerManager) deleteConfig(w http.ResponseWriter, r *http.Request) {
	providerType := serverops.GetPathParam(r, "providerType", "The type of the provider to delete (e.g., 'openai', 'gemini').")
	if providerType == "" {
		_ = serverops.Error(w, r, errors.New("providerType is required in path"), serverops.DeleteOperation)
		return
	}

	if err := p.providerService.DeleteProviderConfig(r.Context(), providerType); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "Provider config deleted successfully") // @response string
}

// Lists all configured external providers with pagination support.
func (p *providerManager) listConfigs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Use the new helper for pagination parameters
	limitStr := serverops.GetQueryParam(r, "limit", "100", "The maximum number of items to return per page.")
	cursorStr := serverops.GetQueryParam(r, "cursor", "", "An optional RFC3339Nano timestamp to fetch the next page of results.")

	// Parse pagination parameters from the retrieved strings
	var cursor *time.Time
	if cursorStr != "" {
		t, err := time.Parse(time.RFC3339Nano, cursorStr)
		if err != nil {
			err = fmt.Errorf("%w: invalid cursor format, expected RFC3339Nano", serverops.ErrUnprocessableEntity)
			_ = serverops.Error(w, r, err, serverops.ListOperation)
			return
		}
		cursor = &t
	}

	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		err = fmt.Errorf("%w: invalid limit format, expected integer", serverops.ErrUnprocessableEntity)
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	configs, err := p.providerService.ListProviderConfigs(ctx, cursor, limit)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.ListOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, configs) // @response []runtimestate.ProviderConfig
}

// Retrieves configuration details for a specific external provider.
func (p *providerManager) get(w http.ResponseWriter, r *http.Request) {
	providerType := serverops.GetPathParam(r, "providerType", "The type of the provider to retrieve (e.g., 'openai', 'gemini').")
	if providerType == "" {
		_ = serverops.Error(w, r, errors.New("providerType is required in path"), serverops.GetOperation)
		return
	}

	config, err := p.providerService.GetProviderConfig(r.Context(), providerType)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.GetOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, config) // @response runtimestate.ProviderConfig
}
