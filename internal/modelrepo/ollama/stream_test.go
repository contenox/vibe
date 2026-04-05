package ollama

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOllamaStreamClient_StreamsThinkingDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/chat", r.URL.Path)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"test","message":{"thinking":"think-1"},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"content":"hello"},"done":true}`)
	}))
	defer srv.Close()

	httpClient, err := newOllamaHTTPClient(srv.URL, "", srv.Client())
	require.NoError(t, err)

	client := &OllamaStreamClient{
		ollamaClient: httpClient,
		modelName:    "test-model",
		tracker:      libtracker.NoopTracker{},
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
