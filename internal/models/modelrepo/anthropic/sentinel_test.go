package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func newSentinelChatClient(url string, client *http.Client) *anthropicChatClient {
	return &anthropicChatClient{anthropicClient: anthropicClient{
		baseURL:    url,
		apiKey:     "k",
		modelName:  "claude-haiku-4-5",
		httpClient: client,
		tracker:    libtracker.NoopTracker{},
	}}
}

func TestUnit_Anthropic_TypedSentinels(t *testing.T) {
	t.Run("prompt too long", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`))
		}))
		defer srv.Close()

		_, err := newSentinelChatClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrContextLengthExceeded)
	})

	t.Run("429 rate limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`))
		}))
		defer srv.Close()

		_, err := newSentinelChatClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrRateLimited)
	})

	t.Run("529 overloaded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
		}))
		defer srv.Close()

		_, err := newSentinelChatClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrRateLimited)
	})
}
