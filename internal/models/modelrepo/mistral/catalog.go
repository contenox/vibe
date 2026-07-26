// Package mistral is a direct (non-Vertex) provider for the Mistral API
// (api.mistral.ai), which speaks the OpenAI-compatible chat/completions format.
// It reuses the shared chatcompletions codec; only the transport (API-key auth,
// vendor base URL) is Mistral-specific.
package mistral

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type catalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
}

func init() {
	modelrepo.RegisterCatalogProvider("mistral", func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		client := opts.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		return &catalogProvider{spec: spec, httpClient: client, tracker: opts.Tracker}, nil
	})
}

func (p *catalogProvider) Type() string { return "mistral" }

func (p *catalogProvider) baseURL() string {
	if base := strings.TrimSpace(p.spec.BaseURL); base != "" {
		return base
	}
	return defaultBaseURL
}

func (p *catalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.baseURL(), "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.spec.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.spec.APIKey)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mistral catalog returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []struct {
			ID              string `json:"id"`
			MaxOutputTokens int    `json:"max_output_tokens"`
			MaxTokens       int    `json:"max_tokens"`
			// Capabilities mirrors Mistral's per-model capability object from
			// GET /v1/models. Only the fields we consume are decoded; Mistral
			// reports the rest (completion_fim, function_calling, fine_tuning,
			// ocr, classification, ...) which we ignore here.
			Capabilities struct {
				// Vision reports whether the model accepts image input. This is
				// the authoritative source for CanVision; never infer it from the
				// model name.
				Vision bool `json:"vision"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode mistral catalog response: %w", err)
	}
	models := make([]modelrepo.ObservedModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		model := inferObservedModel(item.ID)
		model.MaxOutputTokens = item.MaxOutputTokens
		if model.MaxOutputTokens <= 0 {
			model.MaxOutputTokens = item.MaxTokens
		}
		// CanVision comes straight from Mistral's API capability object.
		model.CanVision = item.Capabilities.Vision
		models = append(models, model)
	}
	return models, nil
}

func (p *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewMistralProvider(p.spec.APIKey, model.Name, []string{p.baseURL()}, model.CapabilityConfig, p.httpClient, p.tracker)
}

func inferObservedModel(id string) modelrepo.ObservedModel {
	om := modelrepo.ObservedModel{Name: id}
	if strings.Contains(strings.ToLower(id), "embed") {
		om.CanEmbed = true
		return om
	}
	om.CanChat = true
	om.CanPrompt = true
	om.CanStream = true
	return om
}
