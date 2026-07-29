package vllm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Drives a recorded chat-completions SSE transcript through the real adapter
// and the engine-side assembler, proving the raw-delta contract end to end.
func TestUnit_VLLMStreamClient_GoldenFixture_ToolCallFragments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning":"hmm"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"let me "}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"check"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_v1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Berlin\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`,
		}
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &VLLMStreamClient{
		vLLMClient: vLLMClient{
			baseURL:    srv.URL,
			httpClient: srv.Client(),
			modelName:  "test-model",
			tracker:    libtracker.NoopTracker{},
		},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("vllm", "test-model")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	assert.Equal(t, "let me check", res.Content)
	assert.Equal(t, "hmm", res.Thinking)
	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "call_v1", res.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", res.ToolCalls[0].Function.Name)
	assert.Equal(t, `{"city":"Berlin"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "tool_calls", res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 20, res.Usage.PromptTokens)
	assert.Equal(t, 8, res.Usage.CompletionTokens)
}

// An in-stream error frame ends the stream with an Error parcel.
func TestUnit_VLLMStreamClient_ErrorChunkSurfaces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"error":"engine exploded"}`+"\n\n")
	}))
	defer srv.Close()

	client := &VLLMStreamClient{
		vLLMClient: vLLMClient{
			baseURL:    srv.URL,
			httpClient: srv.Client(),
			modelName:  "test-model",
			tracker:    libtracker.NoopTracker{},
		},
	}

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("vllm", "test-model")
	for parcel := range stream {
		_ = asm.Consume(parcel)
	}
	_, err = asm.Result()
	require.ErrorContains(t, err, "engine exploded")
}
