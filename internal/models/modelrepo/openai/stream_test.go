package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_OpenAIStreamClient_StreamsThinkingDeltas(t *testing.T) {
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

func TestUnit_OpenAIStreamClient_ResponsesAPIStreamsTextDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/responses", r.URL.Path)
		// Verify streaming is requested (not the old blocking path).
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, true, body["stream"], "Responses API must be called with stream:true")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"Hello"}`+"\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":", world"}`+"\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[],"reasoning":{"summary":""}}}`+"\n\n")
	}))
	defer srv.Close()

	client := &OpenAIStreamClient{
		openAIClient: openAIClient{
			baseURL:    srv.URL,
			apiKey:     "key",
			httpClient: srv.Client(),
			modelName:  "gpt-5",
			tracker:    libtracker.NoopTracker{},
		},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)

	var got []string
	for p := range stream {
		require.NoError(t, p.Error)
		if p.Data != "" {
			got = append(got, p.Data)
		}
	}
	require.Equal(t, []string{"Hello", ", world"}, got)
}

func TestUnit_OpenAIStreamClient_ResponsesAPIEmitsReasoningSummary(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"ans"}`+"\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[],"reasoning":{"summary":"I reasoned this"}}}`+"\n\n")
	}))
	defer srv.Close()

	client := &OpenAIStreamClient{
		openAIClient: openAIClient{
			baseURL:    srv.URL,
			apiKey:     "key",
			httpClient: srv.Client(),
			modelName:  "gpt-5",
			tracker:    libtracker.NoopTracker{},
		},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)

	var data, thinking string
	for p := range stream {
		require.NoError(t, p.Error)
		data += p.Data
		thinking += p.Thinking
	}
	require.Equal(t, "ans", data)
	require.Equal(t, "I reasoned this", thinking)
}
