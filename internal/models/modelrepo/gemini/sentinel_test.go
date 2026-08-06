package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// Maps Gemini's error shapes: 400 INVALID_ARGUMENT w/ token phrasing →
// context overflow; 429 RESOURCE_EXHAUSTED → rate limited.
func TestUnit_Gemini_TypedSentinels(t *testing.T) {
	newClient := func(url string, hc *http.Client) *GeminiChatClient {
		return &GeminiChatClient{geminiClient: geminiClient{
			baseURL:    url,
			apiKey:     "k",
			modelName:  "gemini-flash-latest",
			httpClient: hc,
			tracker:    libtracker.NoopTracker{},
		}}
	}

	t.Run("token count exceeded", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"The input token count exceeds the maximum number of tokens allowed 1048576.","status":"INVALID_ARGUMENT"}}`))
		}))
		defer srv.Close()

		_, err := newClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrContextLengthExceeded)
	})

	t.Run("resource exhausted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"You exceeded your current quota.","status":"RESOURCE_EXHAUSTED"}}`))
		}))
		defer srv.Close()

		_, err := newClient(srv.URL, srv.Client()).Chat(context.Background(),
			[]modelrepo.Message{{Role: "user", Content: "hi"}})
		require.ErrorIs(t, err, modelrepo.ErrRateLimited)
	})
}
