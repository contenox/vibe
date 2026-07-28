package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_GeminiStreamClient_StreamsThinkingDeltas(t *testing.T) {
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

	asm := modelrepo.NewStreamAssembler("gemini", "gemini-test")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)
	assert.Equal(t, "think-1", res.Thinking)
	assert.Equal(t, "hello", res.Content)
}

// Drives a recorded SSE transcript through the real adapter and assembler,
// and pins that the stream request carries the hoisted systemInstruction
// exactly like chat.
func TestUnit_GeminiStreamClient_GoldenFixture_ToolCallsSystemAndUsage(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`{"candidates":[{"content":{"parts":[{"text":"pondering","thought":true}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"let me "}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"check"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Berlin"}},"thoughtSignature":"sig-1"},{"functionCall":{"name":"get_time","args":{"tz":"CET"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"candidatesTokenCount":9,"totalTokenCount":24}}`,
		}
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
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

	stream, err := client.Stream(context.Background(), []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "weather?"},
	})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("gemini", "gemini-test")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	si, ok := gotBody["system_instruction"].(map[string]any)
	require.True(t, ok, "stream request must carry systemInstruction (C2), body: %v", gotBody)
	parts := si["parts"].([]any)
	require.Len(t, parts, 1)
	assert.Equal(t, "be terse", parts[0].(map[string]any)["text"])

	assert.Equal(t, "let me check", res.Content)
	assert.Equal(t, "pondering", res.Thinking)
	require.Len(t, res.ToolCalls, 2)
	assert.Equal(t, "get_weather", res.ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"city":"Berlin"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "sig-1", res.ToolCalls[0].ProviderMeta["thought_signature"])
	assert.Equal(t, "get_time", res.ToolCalls[1].Function.Name)
	assert.Equal(t, "sig-1", res.ToolCalls[1].ProviderMeta["thought_signature"], "signature propagates to parallel calls in the same turn")
	assert.Equal(t, "STOP", res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 15, res.Usage.PromptTokens)
	assert.Equal(t, 9, res.Usage.CompletionTokens)
	assert.Equal(t, 24, res.Usage.TotalTokens)
}
