package vertex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func TestUnit_VertexChatClient_Chat(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "), "expected ADC bearer token")
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.True(t, strings.HasSuffix(r.URL.Path, ":generateContent"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vertexResponse{
			Candidates: []struct {
				Content      vertexContent `json:"content"`
				FinishReason string        `json:"finishReason,omitempty"`
			}{
				{Content: vertexContent{
					Role:  "model",
					Parts: []vertexPart{{Text: "hello back"}},
				}},
			},
		})
	}))
	defer srv.Close()

	client := &vertexChatClient{
		vertexClient: vertexClient{
			baseURL:   srv.URL + "/v1/projects/test/locations/us-central1",
			publisher: "google",
			modelName: "gemini-2.5-flash",
			httpClient: &http.Client{
				Transport: bearerInjectTransport{
					serverURL: srv.URL,
					token:     "fake-adc-token",
				},
			},
			tracker: libtracker.NoopTracker{},
			tokenFn: func(_ context.Context) (string, error) { return "fake-adc-token", nil },
		},
	}

	result, err := client.Chat(context.Background(), []modelrepo.Message{
		{Role: "user", Content: "hello"},
	})
	require.NoError(t, err)
	require.Equal(t, "hello back", result.Message.Content)
	require.Equal(t, "assistant", result.Message.Role)
}
