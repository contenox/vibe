// Package anthropic is a direct (non-Vertex) provider for the Anthropic API
// (api.anthropic.com), which speaks the Messages API. It reuses the shared
// messages codec; only the transport (x-api-key + anthropic-version header,
// model in body, vendor base URL) is Anthropic-specific.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
)

type catalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
}

func init() {
	modelrepo.RegisterCatalogProvider("anthropic", func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		client := opts.HTTPClient
		if client == nil {
			client = modelrepo.SharedHTTPClient
		}
		return &catalogProvider{spec: spec, httpClient: client, tracker: opts.Tracker}, nil
	})
}

func (p *catalogProvider) Type() string { return "anthropic" }

func (p *catalogProvider) baseURL() string {
	if base := strings.TrimSpace(p.spec.BaseURL); base != "" {
		return base
	}
	return defaultBaseURL
}

func (p *catalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	// Catalog listing is a non-streaming call: bound it end-to-end.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()

	return p.listModels(ctx, "/v1/models")
}

type modelListResponse struct {
	Data    []modelInfo `json:"data"`
	HasMore bool        `json:"has_more"`
	LastID  string      `json:"last_id"`
}

type modelInfo struct {
	ID              string             `json:"id"`
	CreatedAt       string             `json:"created_at"`
	MaxInputTokens  int                `json:"max_input_tokens"`
	MaxOutputTokens int                `json:"max_output_tokens"`
	MaxTokens       int                `json:"max_tokens"`
	Capabilities    *modelCapabilities `json:"capabilities"`
}

type modelCapabilities struct {
	Thinking   thinkingCapability `json:"thinking"`
	Effort     effortCapability   `json:"effort"`
	ImageInput capabilitySupport  `json:"image_input"`
}

type thinkingCapability struct {
	Supported bool          `json:"supported"`
	Types     thinkingTypes `json:"types"`
}

type thinkingTypes struct {
	Adaptive capabilitySupport `json:"adaptive"`
	Enabled  capabilitySupport `json:"enabled"`
}

type effortCapability struct {
	Supported bool              `json:"supported"`
	Low       capabilitySupport `json:"low"`
	Medium    capabilitySupport `json:"medium"`
	High      capabilitySupport `json:"high"`
	XHigh     capabilitySupport `json:"xhigh"`
	Max       capabilitySupport `json:"max"`
}

type capabilitySupport struct {
	Supported bool `json:"supported"`
}

func (p *catalogProvider) listModels(ctx context.Context, path string) ([]modelrepo.ObservedModel, error) {
	var all []modelrepo.ObservedModel
	afterID := ""
	for {
		url := strings.TrimRight(p.baseURL(), "/") + path
		if afterID != "" {
			url += "?after_id=" + afterID
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if p.spec.APIKey != "" {
			req.Header.Set("x-api-key", p.spec.APIKey)
		}
		req.Header.Set("anthropic-version", anthropicAPIVersion)

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("anthropic catalog %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var payload modelListResponse
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode anthropic catalog response: %w", err)
		}
		for _, item := range payload.Data {
			modifiedAt, _ := time.Parse(time.RFC3339, item.CreatedAt)
			maxOutputTokens := item.MaxOutputTokens
			if maxOutputTokens <= 0 {
				maxOutputTokens = item.MaxTokens
			}
			all = append(all, modelrepo.ObservedModel{
				Name:          item.ID,
				ContextLength: item.MaxInputTokens,
				ModifiedAt:    modifiedAt,
				CapabilityConfig: modelrepo.CapabilityConfig{
					ContextLength:   item.MaxInputTokens,
					MaxOutputTokens: maxOutputTokens,
					CanChat:         true,
					CanStream:       true,
					CanPrompt:       true,
					CanThink:        anthropicCapabilitiesCanThink(item.Capabilities),
					CanVision:       anthropicCapabilitiesCanVision(item.Capabilities),
				},
			})
		}
		if !payload.HasMore {
			break
		}
		afterID = payload.LastID
	}
	return all, nil
}

func anthropicCapabilitiesCanThink(caps *modelCapabilities) bool {
	if caps == nil {
		return false
	}
	thinking := caps.Thinking
	if thinking.Supported || thinking.Types.Adaptive.Supported || thinking.Types.Enabled.Supported {
		return true
	}
	effort := caps.Effort
	return effort.Supported || effort.Low.Supported || effort.Medium.Supported || effort.High.Supported || effort.XHigh.Supported || effort.Max.Supported
}

// anthropicCapabilitiesCanVision reports whether the model accepts image input,
// read from the model resource's capabilities.image_input.supported field
// returned by GET /v1/models (documented as "Whether the model accepts image
// content blocks").
func anthropicCapabilitiesCanVision(caps *modelCapabilities) bool {
	if caps == nil {
		return false
	}
	return caps.ImageInput.Supported
}

func (p *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewAnthropicProvider(p.spec.APIKey, model.Name, []string{p.baseURL()}, model.CapabilityConfig, p.httpClient, p.tracker)
}
