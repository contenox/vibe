package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

func TestUnit_VertexStreamClient_Stream(t *testing.T) {
	t.Parallel()

	chunks := []vertexResponse{
		{Candidates: []struct {
			Content      vertexContent `json:"content"`
			FinishReason string        `json:"finishReason,omitempty"`
		}{
			{Content: vertexContent{Parts: []vertexPart{{Text: "hello "}}}},
		}},
		{Candidates: []struct {
			Content      vertexContent `json:"content"`
			FinishReason string        `json:"finishReason,omitempty"`
		}{
			{Content: vertexContent{Parts: []vertexPart{{Text: "world"}}}},
		}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))
		require.True(t, strings.HasSuffix(r.URL.Path, ":streamGenerateContent"))

		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(b))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	client := &vertexStreamClient{
		vertexClient: vertexClient{
			baseURL:       srv.URL + "/v1/projects/test/locations/us-central1",
			publisher:     "google",
			modelName:     "gemini-2.5-flash",
			contextLength: 0,
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

	stream, err := client.Stream(context.Background(), []modelrepo.Message{
		{Role: "user", Content: "hello"},
	})
	require.NoError(t, err)

	var texts []string
	for parcel := range stream {
		require.NoError(t, parcel.Error)
		if parcel.Data != "" {
			texts = append(texts, parcel.Data)
		}
	}

	require.Equal(t, []string{"hello ", "world"}, texts)
}
