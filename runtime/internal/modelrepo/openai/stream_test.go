package openai

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

func TestOpenAIStreamClient_StreamsThinkingDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think-1\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &OpenAIStreamClient{
		openAIClient: openAIClient{
			baseURL:    srv.URL,
			apiKey:     "test-key",
			httpClient: srv.Client(),
			modelName:  "gpt-test",
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
