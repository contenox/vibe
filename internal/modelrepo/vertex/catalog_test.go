package vertex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_GoogleCatalog_ListModels(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.NotEmpty(t, r.Header.Get("Authorization"), "expected ADC bearer token")
		require.Equal(t, "test-project", r.Header.Get("x-goog-user-project"))
		require.Equal(t, "/v1beta1/publishers/google/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"publisherModels": []map[string]any{
				{"name": "publishers/google/models/gemini-2.5-flash"},
				{"name": "publishers/google/models/gemini-2.5-pro"},
			},
		})
	}))
	defer server.Close()

	catalog := &googleCatalogProvider{
		spec: modelrepo.BackendSpec{
			Type:    "vertex-google",
			BaseURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/test-project/locations/us-central1",
		},
		tokenFn: func(_ context.Context) (string, error) { return "fake-token", nil },
		httpClient: &http.Client{
			Transport: bearerInjectTransport{
				inner:     server.Client().Transport,
				serverURL: server.URL,
				token:     "fake-token",
			},
		},
	}

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 2)

	flash := models[0]
	require.Equal(t, "gemini-2.5-flash", flash.Name)
	require.True(t, flash.CanChat)
	require.True(t, flash.CanPrompt)
	require.True(t, flash.CanStream)

	pro := models[1]
	require.Equal(t, "gemini-2.5-pro", pro.Name)
	require.True(t, pro.CanChat)
	require.False(t, pro.CanEmbed)

	provider := catalog.ProviderFor(flash)
	require.Equal(t, "vertex-google", provider.GetType())
	require.Equal(t, "gemini-2.5-flash", provider.ModelName())
}

func TestUnit_PublisherCatalog_ListModels(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.NotEmpty(t, r.Header.Get("Authorization"), "expected ADC bearer token")
		require.Equal(t, "test-project", r.Header.Get("x-goog-user-project"))
		require.Equal(t, "/v1beta1/publishers/anthropic/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"publisherModels": []map[string]any{
				{"name": "publishers/anthropic/models/claude-sonnet-4-5-20251029"},
				{"name": "publishers/anthropic/models/claude-haiku-4-5"},
			},
		})
	}))
	defer server.Close()

	catalog := &publisherCatalogProvider{
		publisher: "anthropic",
		spec: modelrepo.BackendSpec{
			Type:    "vertex-anthropic",
			BaseURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/test-project/locations/us-central1",
		},
		tokenFn: func(_ context.Context) (string, error) { return "fake-token", nil },
		httpClient: &http.Client{
			Transport: bearerInjectTransport{
				inner:     server.Client().Transport,
				serverURL: server.URL,
				token:     "fake-token",
			},
		},
	}

	models, err := catalog.ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 2)

	require.Equal(t, "claude-sonnet-4-5-20251029", models[0].Name)
	require.Equal(t, "claude-haiku-4-5", models[1].Name)

	// All capabilities must be false (no metadata from publisher API).
	require.False(t, models[0].CanChat)
	require.False(t, models[0].CanStream)
	require.Equal(t, 0, models[0].ContextLength)

	provider := catalog.ProviderFor(models[0])
	require.Equal(t, "vertex-anthropic", provider.GetType())
	require.Equal(t, "claude-sonnet-4-5-20251029", provider.ModelName())
}

// bearerInjectTransport provides a fake bearer token and redirects to the test server.
type bearerInjectTransport struct {
	inner     http.RoundTripper
	serverURL string
	token     string
}

func (t bearerInjectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	cloned.URL.Scheme = "http"
	cloned.URL.Host = t.serverURL[len("http://"):]
	if t.inner != nil {
		return t.inner.RoundTrip(cloned)
	}
	return http.DefaultTransport.RoundTrip(cloned)
}
