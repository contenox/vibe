package vllm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_BuildChatRequest_SerializesImageAsContentParts asserts an image
// attachment becomes the OpenAI content-parts array vLLM's endpoint consumes,
// while a text-only turn stays a bare-string content (unchanged wire shape).
func TestUnit_BuildChatRequest_SerializesImageAsContentParts(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	req, _ := buildChatRequest("vlm", []modelrepo.Message{
		{Role: "user", Content: "just text"},
		{Role: "user", Content: "describe this", Images: []modelrepo.ImagePart{
			{Data: pngBytes, MimeType: "image/png"},
		}},
	}, nil)

	raw, err := json.Marshal(req)
	require.NoError(t, err)

	var got struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got.Messages, 2)
	require.Equal(t, byte('"'), got.Messages[0].Content[0], "text-only content should stay a JSON string")

	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	require.NoError(t, json.Unmarshal(got.Messages[1].Content, &parts))
	require.Len(t, parts, 2)
	require.Equal(t, "text", parts[0].Type)
	require.Equal(t, "image_url", parts[1].Type)
	require.NotNil(t, parts[1].ImageURL)
	require.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes), parts[1].ImageURL.URL)
}

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

func TestUnit_VLLMProvider_DoesNotInferThinkingFromModelName(t *testing.T) {
	provider := NewVLLMProvider("Qwen/Qwen3-32B", []string{"http://localhost:8000"}, nil, modelrepo.CapabilityConfig{}, "", nil)
	require.False(t, provider.CanThink(), "model name alone must not set CanThink")

	provider = NewVLLMProvider("custom", []string{"http://localhost:8000"}, nil, modelrepo.CapabilityConfig{CanThink: true}, "", nil)
	require.True(t, provider.CanThink(), "explicit capability metadata must set CanThink")
}

func TestUnit_BuildChatRequest_MapsThinkingLevels(t *testing.T) {
	t.Parallel()

	tool := modelrepo.Tool{
		Type: "function",
		Function: &modelrepo.FunctionTool{
			Name: "lookup_weather",
		},
	}

	req, _ := buildChatRequest("model", []modelrepo.Message{{Role: "user", Content: "hi"}}, []modelrepo.ChatArgument{
		modelrepo.WithTool(tool),
		modelrepo.WithThink("xhigh"),
	})
	require.Len(t, req.Tools, 1)
	assert.Equal(t, "high", req.ReasoningEffort)
	require.NotNil(t, req.ChatTemplateKwargs)
	assert.Equal(t, true, req.ChatTemplateKwargs["enable_thinking"])

	req, _ = buildChatRequest("model", []modelrepo.Message{{Role: "user", Content: "hi"}}, []modelrepo.ChatArgument{
		modelrepo.WithThink("none"),
	})
	assert.Equal(t, "none", req.ReasoningEffort)
	require.NotNil(t, req.ChatTemplateKwargs)
	assert.Equal(t, false, req.ChatTemplateKwargs["enable_thinking"])

	req, _ = buildChatRequest("model", []modelrepo.Message{{Role: "user", Content: "hi"}}, []modelrepo.ChatArgument{
		modelrepo.WithThink("high"),
	}, false)
	assert.Empty(t, req.ReasoningEffort, "provider with CanThink=false must omit vLLM reasoning effort")
	assert.Nil(t, req.ChatTemplateKwargs, "provider with CanThink=false must omit vLLM chat template thinking kwargs")
}

func TestUnit_VLLMClient_ClampsChatMaxTokens(t *testing.T) {
	t.Parallel()

	req, _ := buildChatRequest("model", []modelrepo.Message{{Role: "user", Content: "hi"}}, []modelrepo.ChatArgument{
		modelrepo.WithMaxTokens(4096),
	})
	client := &vLLMClient{maxOutputTokens: 1024}
	client.clampChatRequest(&req)

	require.NotNil(t, req.MaxTokens)
	assert.Equal(t, 1024, *req.MaxTokens)
}

func TestUnit_VLLMChat_AllowsToolCallsFinishReason(t *testing.T) {
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
			canThink:   true,
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

func TestUnit_VLLMPrompt_TracksReasoningAlias(t *testing.T) {
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

	resp, _, err := client.Prompt(context.Background(), "sys", 0.3, "hello")
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

func TestUnit_VLLMStreamClient_UsesChatRequestParityAndStreamsThinking(t *testing.T) {
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
			canThink:   true,
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
	assert.Equal(t, "high", gotRequest.ReasoningEffort)
	require.NotNil(t, gotRequest.ChatTemplateKwargs)
	assert.Equal(t, true, gotRequest.ChatTemplateKwargs["enable_thinking"])

	asm := modelrepo.NewStreamAssembler("vllm", "test-model")
	for parcel := range stream {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)
	assert.Equal(t, "trace", res.Thinking)
	assert.Equal(t, "hello", res.Content)
}
