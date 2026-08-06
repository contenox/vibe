package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStreamTestClient(srv *httptest.Server) *anthropicStreamClient {
	return &anthropicStreamClient{
		anthropicClient: anthropicClient{
			baseURL:    srv.URL,
			apiKey:     "test-key",
			modelName:  "claude-test",
			httpClient: srv.Client(),
			tracker:    libtracker.NoopTracker{},
		},
	}
}

// Drives a recorded Messages SSE transcript with thinking, split text, and tool_use input_json_delta fragments through the real adapter and assembler end to end.
func TestUnit_AnthropicStreamClient_GoldenFixture_ToolUseFragments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []struct{ name, data string }{
			{"message_start", `{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":25}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"let me "}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"check"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":1}`},
			{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"Berlin\"}"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":2}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":17}}`},
			{"message_stop", `{"type":"message_stop"}`},
		}
		for _, ev := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		}
	}))
	defer srv.Close()

	client := newStreamTestClient(srv)
	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("anthropic", "claude-test")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	assert.Equal(t, "let me check", res.Content)
	assert.Equal(t, "pondering", res.Thinking)
	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "toolu_1", res.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", res.ToolCalls[0].Function.Name)
	assert.Equal(t, `{"city":"Berlin"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "tool_use", res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 25, res.Usage.PromptTokens)
	assert.Equal(t, 17, res.Usage.CompletionTokens)
}

// An in-stream SSE `error` event ends the stream with an Error parcel, and the assembler refuses to produce a result.
func TestUnit_AnthropicStreamClient_ErrorEventSurfaces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n")
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer srv.Close()

	client := newStreamTestClient(srv)
	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("anthropic", "claude-test")
	for parcel := range stream {
		_ = asm.Consume(parcel)
	}
	_, err = asm.Result()
	require.ErrorContains(t, err, "Overloaded")
}
