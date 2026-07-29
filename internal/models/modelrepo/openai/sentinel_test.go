package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func newSentinelChatClient(url string, client *http.Client) *OpenAIChatClient {
	return &OpenAIChatClient{openAIClient: openAIClient{
		baseURL:    url,
		apiKey:     "k",
		httpClient: client,
		modelName:  "gpt-4o",
		tracker:    libtracker.NoopTracker{},
	}}
}

func TestUnit_OpenAI_TypedSentinels(t *testing.T) {
	t.Run("context_length_exceeded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`))
		}))
		defer srv.Close()

		_, err := newSentinelChatClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrContextLengthExceeded)
	})

	t.Run("rate limited 429", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
		}))
		defer srv.Close()

		_, err := newSentinelChatClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrRateLimited)
	})
}
