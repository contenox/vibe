package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

func init() {
	modelrepo.RegisterCatalogProvider("vertex-google", newGoogleCatalog)
	modelrepo.RegisterCatalogProvider("vertex-anthropic", newPublisherCatalog("anthropic"))
	modelrepo.RegisterCatalogProvider("vertex-meta", newPublisherCatalog("meta"))
	modelrepo.RegisterCatalogProvider("vertex-mistralai", newPublisherCatalog("mistralai"))
}

// googleCatalogProvider lists Gemini models via the unauthenticated Gemini AI Studio
// metadata API, which returns full capability and context-length information.
// The same model IDs are valid on Vertex AI at inference time.
type googleCatalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
}

func newGoogleCatalog(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
	if spec.BaseURL == "" {
		return nil, fmt.Errorf("vertex-google backend requires --url with project and location, e.g. https://us-central1-aiplatform.googleapis.com/v1/projects/MY_PROJECT/locations/us-central1")
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &googleCatalogProvider{
		spec:       spec,
		httpClient: client,
		tracker:    opts.Tracker,
	}, nil
}

func (p *googleCatalogProvider) Type() string { return "vertex-google" }

func (p *googleCatalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	const baseListURL = "https://generativelanguage.googleapis.com/v1beta/models"

	var models []modelrepo.ObservedModel
	pageToken := ""

	for {
		url := baseListURL + "?pageSize=100"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
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
			return nil, fmt.Errorf("Gemini model list returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var payload struct {
			Models []struct {
				Name                       string   `json:"name"`
				InputTokenLimit            int      `json:"inputTokenLimit"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode Gemini model list: %w", err)
		}

		for _, item := range payload.Models {
			name := strings.TrimPrefix(item.Name, "models/")
			observed := modelrepo.ObservedModel{
				Name:          name,
				ContextLength: item.InputTokenLimit,
			}
			for _, method := range item.SupportedGenerationMethods {
				switch method {
				case "generateContent":
					observed.CanChat = true
					observed.CanPrompt = true
					observed.CanStream = true
				case "embedContent":
					observed.CanEmbed = true
				}
			}
			models = append(models, observed)
		}

		pageToken = payload.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return models, nil
}

func (p *googleCatalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewVertexProvider("google", model.Name, []string{p.spec.BaseURL}, model.CapabilityConfig, p.spec.APIKey, p.httpClient, p.tracker)
}

// publisherCatalogProvider lists models from the Vertex AI publisher endpoint.
// The API returns model names only — no capability metadata.
// Models are returned with all capability flags false and ContextLength=0;
// users must declare them via `contenox model register` to make them usable.
type publisherCatalogProvider struct {
	publisher  string
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
	tokenFn    func(context.Context) (string, error) // test hook
}

func newPublisherCatalog(publisher string) func(modelrepo.BackendSpec, modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
	return func(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
		if spec.BaseURL == "" {
			return nil, fmt.Errorf("vertex-%s backend requires --url with project and location, e.g. https://us-central1-aiplatform.googleapis.com/v1/projects/MY_PROJECT/locations/us-central1", publisher)
		}
		client := opts.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}
		return &publisherCatalogProvider{
			publisher:  publisher,
			spec:       spec,
			httpClient: client,
			tracker:    opts.Tracker,
			tokenFn:    BearerToken,
		}, nil
	}
}

func (p *publisherCatalogProvider) Type() string { return "vertex-" + p.publisher }

func (p *publisherCatalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	const listBaseURL = "https://aiplatform.googleapis.com/v1beta1/publishers/%s/models"

	var models []modelrepo.ObservedModel
	pageToken := ""

	tokenFn := p.tokenFn
	if tokenFn == nil {
		tokenFn = func(ctx context.Context) (string, error) {
			return BearerTokenWithCreds(ctx, p.spec.APIKey)
		}
	}
	token, err := tokenFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex-%s list models: %w", p.publisher, err)
	}

	for {
		url := fmt.Sprintf(listBaseURL, p.publisher) + "?pageSize=100"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)

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
			return nil, fmt.Errorf("vertex publisher list returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var payload struct {
			PublisherModels []struct {
				Name string `json:"name"`
			} `json:"publisherModels"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode vertex publisher model list: %w", err)
		}

		for _, item := range payload.PublisherModels {
			// Name format: "publishers/{publisher}/models/{model-id}"
			name := item.Name
			if idx := strings.LastIndex(name, "/"); idx >= 0 {
				name = name[idx+1:]
			}
			// All capability flags are false; ContextLength is 0.
			// Users must declare models via `contenox model register` to enable them.
			models = append(models, modelrepo.ObservedModel{Name: name})
		}

		pageToken = payload.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return models, nil
}

func (p *publisherCatalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewVertexProvider(p.publisher, model.Name, []string{p.spec.BaseURL}, model.CapabilityConfig, p.spec.APIKey, p.httpClient, p.tracker)
}
