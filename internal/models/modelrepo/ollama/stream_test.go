package ollama

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_OllamaStreamClient_StreamsThinkingDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/chat", r.URL.Path)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"test","message":{"thinking":"think-1"},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"content":"hello"},"done":true,"done_reason":"stop"}`)
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

	asm := modelrepo.NewStreamAssembler("ollama", "test-model")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)
	assert.Equal(t, "think-1", res.Thinking)
	assert.Equal(t, "hello", res.Content)
	assert.Equal(t, "stop", res.FinishReason)
}

// Drives a recorded NDJSON transcript through the real adapter and the
// engine-side assembler, proving the raw-delta contract end to end.
func TestUnit_OllamaStreamClient_GoldenFixture_ToolCallsUsageTerminal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/chat", r.URL.Path)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"test","message":{"thinking":"pondering"},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"content":"let me "},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"content":"check"},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{"tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Berlin"}}},{"function":{"name":"get_time","arguments":{"tz":"CET"}}}]},"done":false}`)
		fmt.Fprintln(w, `{"model":"test","message":{},"done":true,"done_reason":"stop","prompt_eval_count":12,"eval_count":34}`)
	}))
	defer srv.Close()

	httpClient, err := newOllamaHTTPClient(srv.URL, "", srv.Client())
	require.NoError(t, err)

	client := &OllamaStreamClient{
		ollamaClient: httpClient,
		modelName:    "test-model",
		tracker:      libtracker.NoopTracker{},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("ollama", "test-model")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	assert.Equal(t, "let me check", res.Content)
	assert.Equal(t, "pondering", res.Thinking)
	require.Len(t, res.ToolCalls, 2)
	assert.Equal(t, "get_weather", res.ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"city":"Berlin"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "get_time", res.ToolCalls[1].Function.Name)
	assert.JSONEq(t, `{"tz":"CET"}`, res.ToolCalls[1].Function.Arguments)
	assert.Equal(t, "stop", res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 12, res.Usage.PromptTokens)
	assert.Equal(t, 34, res.Usage.CompletionTokens)
}

// A connection that ends without done=true must surface as an error.
func TestUnit_OllamaStreamClient_TruncatedStreamIsNotSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"test","message":{"content":"partial"},"done":false}`)
	}))
	defer srv.Close()

	httpClient, err := newOllamaHTTPClient(srv.URL, "", srv.Client())
	require.NoError(t, err)

	client := &OllamaStreamClient{
		ollamaClient: httpClient,
		modelName:    "test-model",
		tracker:      libtracker.NoopTracker{},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("ollama", "test-model")
	for parcel := range stream {
		_ = asm.Consume(parcel)
	}
	_, err = asm.Result()
	require.Error(t, err)
}
