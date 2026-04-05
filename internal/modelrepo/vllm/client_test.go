package vllm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturedChange struct {
	id   string
	data any
}

type captureTracker struct {
	mu      sync.Mutex
	changes []capturedChange
}

func (t *captureTracker) Start(
	ctx context.Context,
	operation string,
	subject string,
	kvArgs ...any,
) (func(error), func(string, any), func()) {
	return func(error) {}, func(id string, data any) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.changes = append(t.changes, capturedChange{id: id, data: data})
	}, func() {}
}

func (t *captureTracker) change(id string) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.changes) - 1; i >= 0; i-- {
		if t.changes[i].id == id {
			return t.changes[i].data, true
		}
	}
	return nil, false
}

var _ libtracker.ActivityTracker = (*captureTracker)(nil)

func TestBuildChatRequest_MapsThinkingLevels(t *testing.T) {
	t.Parallel()

	tool := modelrepo.Tool{
		Type: "function",
		Function: &modelrepo.FunctionTool{
			Name: "lookup_weather",
		},
	}

	req := buildChatRequest("model", []modelrepo.Message{{Role: "user", Content: "hi"}}, []modelrepo.ChatArgument{
		modelrepo.WithTool(tool),
		modelrepo.WithThink("xhigh"),
	})
	require.Len(t, req.Tools, 1)
	require.NotNil(t, req.ExtraBody)
	assert.Equal(t, true, req.ExtraBody["chat_template_kwargs"].(map[string]any)["enable_thinking"])

	req = buildChatRequest("model", []modelrepo.Message{{Role: "user", Content: "hi"}}, []modelrepo.ChatArgument{
		modelrepo.WithThink("none"),
	})
	require.NotNil(t, req.ExtraBody)
	assert.Equal(t, false, req.ExtraBody["chat_template_kwargs"].(map[string]any)["enable_thinking"])
}

func TestVLLMChat_AllowsToolCallsFinishReason(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "",
					"reasoning": "trace",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "lookup_weather", "arguments": "{\"city\":\"Berlin\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}]
		}`))
	}))
	defer srv.Close()

	client := &VLLMChatClient{
		vLLMClient: vLLMClient{
			baseURL:    srv.URL,
			httpClient: srv.Client(),
			modelName:  "test-model",
			tracker:    libtracker.NoopTracker{},
		},
	}

	resp, err := client.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "weather?"}})
	require.NoError(t, err)
	assert.Equal(t, "assistant", resp.Message.Role)
	assert.Equal(t, "trace", resp.Message.Thinking)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "lookup_weather", resp.ToolCalls[0].Function.Name)
	assert.Equal(t, "{\"city\":\"Berlin\"}", resp.ToolCalls[0].Function.Arguments)
}

func TestVLLMPrompt_TracksReasoningAlias(t *testing.T) {
	t.Parallel()

	tracker := &captureTracker{}
	var gotRequest chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotRequest))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "final answer",
					"reasoning": "trace"
				},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}`))
	}))
	defer srv.Close()

	client := &vLLMClient{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
		modelName:  "test-model",
		maxTokens:  99,
		tracker:    tracker,
	}

	resp, err := client.Prompt(context.Background(), "sys", 0.3, "hello")
	require.NoError(t, err)
	assert.Equal(t, "final answer", resp)
	require.NotNil(t, gotRequest.Temperature)
	assert.InDelta(t, 0.3, *gotRequest.Temperature, 0.000001)
	require.NotNil(t, gotRequest.MaxTokens)
	assert.Equal(t, 99, *gotRequest.MaxTokens)

	changeData, ok := tracker.change("prompt_completed")
	require.True(t, ok)
	changeMap, ok := changeData.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 5, changeMap["thinking_length"])
}

func TestVLLMStreamClient_UsesChatRequestParityAndStreamsThinking(t *testing.T) {
	t.Parallel()

	tool := modelrepo.Tool{
		Type: "function",
		Function: &modelrepo.FunctionTool{
			Name: "lookup_weather",
		},
	}

	var gotRequest chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotRequest))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"trace\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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

	stream, err := client.Stream(context.Background(), []modelrepo.Message{{Role: "user", Content: "hello"}},
		modelrepo.WithTool(tool),
		modelrepo.WithThink("xhigh"),
	)
	require.NoError(t, err)

	require.True(t, gotRequest.Stream)
	require.Len(t, gotRequest.Tools, 1)
	require.NotNil(t, gotRequest.ExtraBody)
	assert.Equal(t, true, gotRequest.ExtraBody["chat_template_kwargs"].(map[string]any)["enable_thinking"])

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
	assert.Equal(t, "trace", parcels[0].Thinking)
	assert.Equal(t, "hello", parcels[1].Data)
	assert.Equal(t, "", parcels[1].Thinking)
}
