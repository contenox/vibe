package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/runtime/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiStreamClient_StreamsThinkingDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1beta/models/gemini-test:streamGenerateContent", r.URL.Path)
		assert.Equal(t, "sse", r.URL.Query().Get("alt"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"think-1\",\"thought\":true}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
	}))
	defer srv.Close()

	client := &GeminiStreamClient{
		geminiClient: geminiClient{
			apiKey:     "test-key",
			modelName:  "gemini-test",
			baseURL:    srv.URL,
			httpClient: srv.Client(),
			tracker:    libtracker.NoopTracker{},
		},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hello"}})
	require.NoError(t, err)

	var parcels []struct {
		Data     string
		Thinking string
	}
	for parcel := range stream {
		require.NoError(t, parcel.Error)
		parcels = append(parcels, struct {
			Data     string
			Thinking string
		}{
			Data:     parcel.Data,
			Thinking: parcel.Thinking,
		})
	}

	require.Len(t, parcels, 2)
	assert.Equal(t, "", parcels[0].Data)
	assert.Equal(t, "think-1", parcels[0].Thinking)
	assert.Equal(t, "hello", parcels[1].Data)
	assert.Equal(t, "", parcels[1].Thinking)
}
