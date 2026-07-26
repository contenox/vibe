package openrouter

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

const defaultBaseURL = "https://openrouter.ai/api/v1"

type catalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
}

func init() {
	modelrepo.RegisterCatalogProvider("openrouter", func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		client := opts.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		return &catalogProvider{spec: spec, httpClient: client, tracker: opts.Tracker}, nil
	})
}

func (p *catalogProvider) Type() string { return "openrouter" }

func (p *catalogProvider) baseURL() string {
	if base := strings.TrimSpace(p.spec.BaseURL); base != "" {
		return base
	}
	return defaultBaseURL
}

// orModel is the wire shape of one model from GET /api/v1/models.
type orModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		Modality        string   `json:"modality"`
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
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
		return nil, fmt.Errorf("openrouter catalog returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []orModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode openrouter catalog response: %w", err)
	}
	models := make([]modelrepo.ObservedModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		if om, ok := toObservedModel(item); ok {
			models = append(models, om)
		}
	}
	return models, nil
}

func (p *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return newOpenRouterProvider(p.spec.APIKey, model.Name, p.baseURL(), model.CapabilityConfig, p.httpClient, p.tracker)
}

// toObservedModel converts an OpenRouter model entry into an ObservedModel.
// Models that cannot generate text (image/audio generation, video) are dropped.
func toObservedModel(m orModel) (modelrepo.ObservedModel, bool) {
	modality := strings.ToLower(m.Architecture.Modality)
	lower := strings.ToLower(m.ID)

	// Prefer top_provider.context_length when available (it is the effective window
	// for the underlying provider rather than the envelope OpenRouter supports).
	ctxLen := m.TopProvider.ContextLength
	if ctxLen == 0 {
		ctxLen = m.ContextLength
	}

	om := modelrepo.ObservedModel{
		Name:          m.ID,
		ContextLength: ctxLen,
	}
	om.MaxOutputTokens = m.TopProvider.MaxCompletionTokens

	switch {
	case strings.Contains(lower, "embed"):
		om.CanEmbed = true
	case strings.HasSuffix(modality, "->text"), strings.Contains(modality, "->text"):
		om.CanChat = true
		om.CanPrompt = true
		om.CanStream = true
		om.CanVision = orAcceptsImageInput(m)
	default:
		// Non-text-generation models (image, audio, video) — skip.
		return modelrepo.ObservedModel{}, false
	}
	return om, true
}

// orAcceptsImageInput reports whether the model takes image input, read from
// the provider's own metadata: the architecture.input_modalities list when
// present, else the legacy "input->output" modality string's input side.
func orAcceptsImageInput(m orModel) bool {
	for _, mod := range m.Architecture.InputModalities {
		if strings.EqualFold(strings.TrimSpace(mod), "image") {
			return true
		}
	}
	if in, _, ok := strings.Cut(strings.ToLower(m.Architecture.Modality), "->"); ok {
		return strings.Contains(in, "image")
	}
	return false
}
