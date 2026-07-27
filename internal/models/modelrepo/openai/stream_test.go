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

	asm := modelrepo.NewStreamAssembler("openai", "gpt-test")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)
	assert.Equal(t, "think-1", res.Thinking)
	assert.Equal(t, "hello", res.Content)
}

// TestUnit_OpenAIStreamClient_GoldenFixture_ToolCallFragments drives a
// recorded chat-completions SSE transcript — thinking, split content,
// tool-call fragments across chunks for TWO parallel calls, finish_reason and
// the trailing usage chunk — through the real adapter and the engine-side
// assembler, proving the raw-delta contract end to end.
func TestUnit_OpenAIStreamClient_GoldenFixture_ToolCallFragments(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, true, body["stream"])
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hmm"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"let me "}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"check"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Berlin\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"CET\"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":22,"total_tokens":62}}`,
		}
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
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

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("openai", "gpt-test")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	assert.Equal(t, "let me check", res.Content)
	assert.Equal(t, "hmm", res.Thinking)
	require.Len(t, res.ToolCalls, 2)
	assert.Equal(t, "call_a", res.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", res.ToolCalls[0].Function.Name)
	assert.Equal(t, `{"city":"Berlin"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "call_b", res.ToolCalls[1].ID)
	assert.Equal(t, "get_time", res.ToolCalls[1].Function.Name)
	assert.Equal(t, `{"tz":"CET"}`, res.ToolCalls[1].Function.Arguments)
	assert.Equal(t, "tool_calls", res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 40, res.Usage.PromptTokens)
	assert.Equal(t, 22, res.Usage.CompletionTokens)
	assert.Equal(t, 62, res.Usage.TotalTokens)
}

// TestUnit_OpenAIStreamClient_ResponsesGoldenFixture_FunctionCallDeltas locks
// in the Responses-API stream path: function_call argument deltas are emitted
// (they used to be a no-op), reasoning summary deltas stream as thinking, and
// response.completed becomes the typed terminal parcel with usage.
func TestUnit_OpenAIStreamClient_ResponsesGoldenFixture_FunctionCallDeltas(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/responses", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{
			`{"type":"response.reasoning_summary_text.delta","delta":"thinking hard"}`,
			`{"type":"response.output_text.delta","delta":"calling "}`,
			`{"type":"response.output_text.delta","delta":"a tool"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_r1","name":"get_weather","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"Berlin\"}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_r1","name":"get_weather","arguments":"{\"city\":\"Berlin\"}"}}`,
			`{"type":"response.completed","response":{"output":[],"reasoning":{"summary":""},"usage":{"input_tokens":30,"output_tokens":11,"total_tokens":41}}}`,
		}
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
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

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)

	asm := modelrepo.NewStreamAssembler("openai", "gpt-5")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	assert.Equal(t, "calling a tool", res.Content)
	assert.Equal(t, "thinking hard", res.Thinking)
	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "call_r1", res.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", res.ToolCalls[0].Function.Name)
	assert.Equal(t, `{"city":"Berlin"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "stop", res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 30, res.Usage.PromptTokens)
	assert.Equal(t, 11, res.Usage.CompletionTokens)
}

// TestUnit_OpenAIStreamClient_ErrorEventSurfaces: an SSE `error` event ends
// the stream with an Error parcel, and the assembler refuses to produce a
// result from it.
func TestUnit_OpenAIStreamClient_ErrorEventSurfaces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"par"}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"error","code":"server_error","message":"boom"}`+"\n\n")
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

	asm := modelrepo.NewStreamAssembler("openai", "gpt-5")
	var sawErr bool
	for parcel := range stream {
		if asm.Consume(parcel) != nil {
			sawErr = true
		}
	}
	require.True(t, sawErr, "the SSE error event must surface as an Error parcel")
	_, err = asm.Result()
	require.ErrorContains(t, err, "boom")
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
