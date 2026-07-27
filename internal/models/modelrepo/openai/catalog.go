package openai

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

const defaultBaseURL = "https://api.openai.com/v1"

type catalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
}

func init() {
	modelrepo.RegisterCatalogProvider("openai", func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		return &catalogProvider{
			spec:       spec,
			httpClient: opts.HTTPClient,
			tracker:    opts.Tracker,
		}, nil
	})
}

func (p *catalogProvider) Type() string {
	return "openai"
}

func (p *catalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	// Catalog listing is a non-streaming call: bound it end-to-end.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()

	type modelItem struct {
		ID string `json:"id"`
	}
	type modelsPage struct {
		Data    []modelItem `json:"data"`
		HasMore bool        `json:"has_more"`
		LastID  string      `json:"last_id"`
	}

	var all []modelrepo.ObservedModel
	afterID := ""
	for {
		url := strings.TrimRight(p.baseURL(), "/") + "/models"
		if afterID != "" {
			url += "?after=" + afterID
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OpenAI catalog returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var page modelsPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode OpenAI catalog response: %w", err)
		}
		for _, item := range page.Data {
			all = append(all, inferObservedModel(item.ID))
		}
		if !page.HasMore {
			break
		}
		afterID = page.LastID
	}
	return all, nil
}

func (p *catalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewOpenAIProvider(
		p.spec.APIKey,
		model.Name,
		[]string{p.baseURL()},
		model.CapabilityConfig,
		p.httpClient,
		p.tracker,
	)
}

func (p *catalogProvider) baseURL() string {
	base := strings.TrimSpace(p.spec.BaseURL)
	if base == "" {
		return defaultBaseURL
	}
	return base
}

func inferObservedModel(id string) modelrepo.ObservedModel {
	lower := strings.ToLower(id)
	observed := modelrepo.ObservedModel{
		Name:          id,
		ContextLength: 0, // unknown; resolver treats 0 as "do not filter on context"
	}
	observed.MaxOutputTokens = inferOpenAIMaxOutputTokens(id)

	switch {
	case strings.HasPrefix(lower, "text-embedding-"):
		observed.CanEmbed = true
	case strings.Contains(lower, "-instruct"),
		strings.HasPrefix(lower, "davinci-"),
		strings.HasPrefix(lower, "babbage-"):
		observed.CanPrompt = true
	case strings.HasPrefix(lower, "dall-e-"),
		strings.HasPrefix(lower, "sora-"),
		strings.HasPrefix(lower, "chatgpt-image-"),
		strings.Contains(lower, "-image-") && !strings.HasPrefix(lower, "gpt-image-"):
	case strings.HasPrefix(lower, "gpt-image-"):
	case strings.HasPrefix(lower, "tts-"),
		strings.HasSuffix(lower, "-tts"),
		strings.Contains(lower, "-tts-"),
		strings.HasPrefix(lower, "whisper-"),
		strings.Contains(lower, "-audio-"),
		strings.HasPrefix(lower, "gpt-audio"),
		strings.HasPrefix(lower, "gpt-realtime"),
		strings.Contains(lower, "-realtime-"),
		strings.Contains(lower, "-transcribe"),
		strings.HasPrefix(lower, "omni-"):
	case strings.HasPrefix(lower, "gpt-"),
		strings.HasPrefix(lower, "o1"),
		strings.HasPrefix(lower, "o3"),
		strings.HasPrefix(lower, "o4"):
		observed.CanChat = true
		observed.CanPrompt = true
		observed.CanStream = true
	default:
		observed.CanChat = true
		observed.CanPrompt = true
		observed.CanStream = true
	}

	// OpenAI's model API reports no modality, so vision is a maintained list;
	// gated on CanChat so audio/embedding/image-gen models never claim it.
	observed.CanVision = observed.CanChat && modelrepo.OpenAIModelSupportsVision(id)

	return observed
}

func inferOpenAIMaxOutputTokens(id string) int {
	lower := strings.ToLower(strings.TrimSpace(id))
	switch {
	case lower == "gpt-5-chat-latest" || strings.HasPrefix(lower, "gpt-5-chat-"):
		return 16384
	case lower == "gpt-5-pro" || strings.HasPrefix(lower, "gpt-5-pro-"):
		return 272000
	case lower == "gpt-5" ||
		strings.HasPrefix(lower, "gpt-5-202") ||
		strings.HasPrefix(lower, "gpt-5-mini") ||
		strings.HasPrefix(lower, "gpt-5-nano") ||
		strings.HasPrefix(lower, "gpt-5.1") ||
		strings.HasPrefix(lower, "gpt-5.2") ||
		strings.HasPrefix(lower, "gpt-5.4") ||
		strings.HasPrefix(lower, "gpt-5.5"):
		return 128000
	default:
		return 0
	}
}
