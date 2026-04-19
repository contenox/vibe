package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
)

func init() {
	modelrepo.RegisterCatalogProvider("vertex-google", newGoogleCatalog)
	modelrepo.RegisterCatalogProvider("vertex-anthropic", newPublisherCatalog("anthropic"))
	modelrepo.RegisterCatalogProvider("vertex-meta", newPublisherCatalog("meta"))
	modelrepo.RegisterCatalogProvider("vertex-mistralai", newPublisherCatalog("mistralai"))
}

// googleCatalogProvider lists models via the Vertex AI publisher Model Garden API
// (same regional host as the backend URL; ADC or stored service account JSON as for inference).
type googleCatalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
	tokenFn    func(context.Context) (string, error) // test hook; nil → BearerTokenWithCreds
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
	return p.listGoogleModelsFromVertexPublisher(ctx)
}

func (p *googleCatalogProvider) listGoogleModelsFromVertexPublisher(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	tokenFn := p.tokenFn
	if tokenFn == nil {
		tokenFn = func(ctx context.Context) (string, error) {
			return BearerTokenWithCreds(ctx, p.spec.APIKey)
		}
	}
	names, err := listVertexPublisherModelNames(ctx, p.spec.BaseURL, "google", p.httpClient, tokenFn)
	if err != nil {
		return nil, err
	}
	out := make([]modelrepo.ObservedModel, 0, len(names))
	for _, name := range names {
		out = append(out, enrichGooglePublisherModel(name))
	}
	return out, nil
}

// enrichGooglePublisherModel sets coarse capabilities when only the publisher model ID is known.
func enrichGooglePublisherModel(name string) modelrepo.ObservedModel {
	n := strings.ToLower(name)
	om := modelrepo.ObservedModel{Name: name}
	switch {
	case strings.Contains(n, "embed"):
		om.CanEmbed = true
	case strings.Contains(n, "imagen") || strings.Contains(n, "veo-") || strings.Contains(n, "tts") ||
		strings.Contains(n, "lyria") || strings.Contains(n, "nano-banana") || strings.Contains(n, "aqa"):
		// Media / non-chat; leave capabilities off — user can register overrides.
	default:
		om.CanChat = true
		om.CanPrompt = true
		om.CanStream = true
	}
	return om
}

// vertexRegionalPublisherListURL builds the REST URL for listing Model Garden publisher models.
// The API is GET https://{service-endpoint}/v1beta1/publishers/{publisher}/models (regional host
// such as us-central1-aiplatform.googleapis.com), not under .../v1/projects/.../locations/...
// (that path is for inference and returns 404 for list).
func vertexRegionalPublisherListURL(vertexLocationBaseURL, publisher string) (string, error) {
	base := strings.TrimSpace(vertexLocationBaseURL)
	if base == "" {
		return "", fmt.Errorf("empty backend base URL")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse backend base URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("backend base URL has no host")
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/v1beta1/publishers/%s/models", scheme, u.Host, publisher), nil
}

// listVertexPublisherModelNames returns model IDs from the Vertex AI publisher list using the
// regional hostname from the backend URL (same host used for generateContent).
func listVertexPublisherModelNames(ctx context.Context, vertexLocationBaseURL, publisher string, httpClient *http.Client, tokenFn func(context.Context) (string, error)) ([]string, error) {
	listURLPrefix, err := vertexRegionalPublisherListURL(vertexLocationBaseURL, publisher)
	if err != nil {
		return nil, fmt.Errorf("vertex-%s list models: %w", publisher, err)
	}

	token, err := tokenFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex-%s list models: %w", publisher, err)
	}

	var names []string
	pageToken := ""

	for {
		url := listURLPrefix + "?pageSize=100"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if project := extractProjectFromVertexURL(vertexLocationBaseURL); project != "" {
			req.Header.Set("x-goog-user-project", project)
		}

		resp, err := httpClient.Do(req)
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
			name := item.Name
			if idx := strings.LastIndex(name, "/"); idx >= 0 {
				name = name[idx+1:]
			}
			names = append(names, name)
		}

		pageToken = payload.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return names, nil
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
	tokenFn := p.tokenFn
	if tokenFn == nil {
		tokenFn = func(ctx context.Context) (string, error) {
			return BearerTokenWithCreds(ctx, p.spec.APIKey)
		}
	}
	names, err := listVertexPublisherModelNames(ctx, p.spec.BaseURL, p.publisher, p.httpClient, tokenFn)
	if err != nil {
		return nil, err
	}
	models := make([]modelrepo.ObservedModel, 0, len(names))
	for _, name := range names {
		// All capability flags are false; ContextLength is 0.
		// Users must declare models via `contenox model register` to enable them.
		models = append(models, modelrepo.ObservedModel{Name: name})
	}
	return models, nil
}

func (p *publisherCatalogProvider) ProviderFor(model modelrepo.ObservedModel) modelrepo.Provider {
	return NewVertexProvider(p.publisher, model.Name, []string{p.spec.BaseURL}, model.CapabilityConfig, p.spec.APIKey, p.httpClient, p.tracker)
}
