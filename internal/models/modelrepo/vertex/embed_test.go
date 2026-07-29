package vertex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// Pins the Vertex text-embedding wire shape: POST .../models/{model}:predict
// with instances[].content; values read from predictions[0].embeddings.values.
func TestUnit_VertexEmbedClient_Embed(t *testing.T) {
	t.Parallel()

	var got vertexPredictEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))
		require.True(t, strings.HasSuffix(r.URL.Path, "/publishers/google/models/gemini-embedding-001:predict"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"predictions":[{"embeddings":{"values":[0.25,-0.5,0.75],"statistics":{"token_count":3}}}]}`))
	}))
	defer srv.Close()

	client := &vertexEmbedClient{
		vertexClient: vertexClient{
			baseURL:    srv.URL + "/v1/projects/test/locations/us-central1",
			publisher:  "google",
			modelName:  "gemini-embedding-001",
			httpClient: srv.Client(),
			tracker:    libtracker.NoopTracker{},
			tokenFn:    func(context.Context) (string, error) { return "fake-adc-token", nil },
		},
	}

	vec, err := client.Embed(context.Background(), "hello world")
	require.NoError(t, err)
	require.Equal(t, []float64{0.25, -0.5, 0.75}, vec)
	require.Len(t, got.Instances, 1)
	require.Equal(t, "hello world", got.Instances[0].Content)
}

// TestUnit_VertexEmbedClient_NoValues asserts a teaching error when the model
// returns no embedding values (e.g. a chat model was addressed with :predict).
func TestUnit_VertexEmbedClient_NoValues(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"predictions":[]}`))
	}))
	defer srv.Close()

	client := &vertexEmbedClient{
		vertexClient: vertexClient{
			baseURL:    srv.URL + "/v1/projects/test/locations/us-central1",
			publisher:  "google",
			modelName:  "gemini-flash-latest",
			httpClient: srv.Client(),
			tracker:    libtracker.NoopTracker{},
			tokenFn:    func(context.Context) (string, error) { return "fake-adc-token", nil },
		},
	}

	_, err := client.Embed(context.Background(), "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "gemini-embedding-001", "error must teach which kind of model to use")
}

// Advertised models connect; others refuse with a teaching error instead of
// failing at request time.
func TestUnit_VertexProvider_EmbedConnectionGate(t *testing.T) {
	t.Parallel()

	embedder := NewVertexProvider("google", "gemini-embedding-001", []string{"https://example.test/v1/projects/p/locations/l"},
		modelrepo.CapabilityConfig{CanEmbed: true}, "", nil, nil)
	client, err := embedder.GetEmbedConnection(context.Background(), "")
	require.NoError(t, err)
	require.NotNil(t, client)

	chatOnly := NewVertexProvider("google", "gemini-flash-latest", []string{"https://example.test/v1/projects/p/locations/l"},
		modelrepo.CapabilityConfig{CanChat: true}, "", nil, nil)
	_, err = chatOnly.GetEmbedConnection(context.Background(), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support embeddings")
}
