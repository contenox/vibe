package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/contenox/beam/internal/libtracker"

	"github.com/contenox/beam/internal/models/modelrepo"
)

func init() {
	modelrepo.RegisterCatalogProvider("vertex-google", newGoogleCatalog)
}

// googleCatalogProvider lists models via the Vertex AI publisher Model Garden API
// (same regional host as the backend URL; ADC or stored service account JSON as for inference).
type googleCatalogProvider struct {
	spec       modelrepo.BackendSpec
	httpClient *http.Client
	tracker    libtracker.ActivityTracker
	tokenFn    func(context.Context) (string, error) // test tools; nil → BearerTokenWithCreds
}

func newGoogleCatalog(spec modelrepo.BackendSpec, opts modelrepo.CatalogOptions) (modelrepo.CatalogProvider, error) {
	if spec.BaseURL == "" {
		return nil, fmt.Errorf("vertex-google backend requires --url with project and location, e.g. https://us-central1-aiplatform.googleapis.com/v1/projects/MY_PROJECT/locations/us-central1")
	}
	client := opts.HTTPClient
	if client == nil {
		client = modelrepo.SharedHTTPClient
	}
	return &googleCatalogProvider{
		spec:       spec,
		httpClient: client,
		tracker:    opts.Tracker,
	}, nil
}

func (p *googleCatalogProvider) Type() string { return "vertex-google" }

func (p *googleCatalogProvider) ListModels(ctx context.Context) ([]modelrepo.ObservedModel, error) {
	// Catalog listing is a non-streaming call: bound it end-to-end.
	ctx, cancel := modelrepo.NonStreamingContext(ctx)
	defer cancel()

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
		// The Vertex publisher API reports no input modalities, so vision comes
		// from the hand-maintained Google allowlist rather than runtime detection.
		om.CanVision = modelrepo.GeminiModelSupportsVision(name)
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

// vertexPublisherModel is one entry from the Vertex Model Garden publisher list.
// listVertexPublisherModelNames returns model IDs from the Vertex AI publisher
// list using the regional hostname from the backend URL (same host used for
// generateContent).
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
