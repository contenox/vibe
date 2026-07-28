package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// TestUnit_VLLM_TypedSentinels maps vLLM's OpenAI-style token-limit error
// string onto the context-overflow sentinel.
func TestUnit_VLLM_TypedSentinels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens.","type":"BadRequestError","code":400}`))
	}))
	defer srv.Close()

	client := &VLLMChatClient{vLLMClient: vLLMClient{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
		modelName:  "Qwen/Qwen3-32B",
		tracker:    libtracker.NoopTracker{},
	}}

	_, err := client.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})
	require.ErrorIs(t, err, modelrepo.ErrContextLengthExceeded)
}

// Engine-qualified tool names ("toolsName.toolName") are sanitized in both
// the tool schemas and prior-turn assistant tool calls, and translated back.
func TestUnit_VLLM_ToolNameSanitization(t *testing.T) {
	fn := &modelrepo.FunctionTool{Name: "filesystem.list_directory"}
	cfg := &modelrepo.ChatConfig{Tools: []modelrepo.Tool{{Type: "function", Function: fn}}}

	history := []modelrepo.Message{
		{Role: "user", Content: "list my files"},
	}
	priorCall := modelrepo.ToolCall{ID: "t1", Type: "function"}
	priorCall.Function.Name = "filesystem.list_directory"
	priorCall.Function.Arguments = "{}"
	history = append(history,
		modelrepo.Message{Role: "assistant", ToolCalls: []modelrepo.ToolCall{priorCall}},
		modelrepo.Message{Role: "tool", ToolCallID: "t1", Content: "[]"},
	)

	req, nameMap := buildChatRequestFromConfig("m", history, cfg)

	require.Len(t, req.Tools, 1)
	require.Equal(t, "filesystem_list_directory", req.Tools[0].Function.Name)
	require.Equal(t, "filesystem.list_directory", nameMap["filesystem_list_directory"])

	assistant, ok := req.Messages[1].(vllmWireMessage)
	require.True(t, ok, "non-image messages serialize as the explicit wire struct, never the neutral Message")
	require.Equal(t, "filesystem_list_directory", assistant.ToolCalls[0].Function.Name,
		"history tool calls must carry the sanitized name too")

	wireCall := chatToolCall{ID: "t2", Type: "function"}
	wireCall.Function.Name = "filesystem_list_directory"
	wireCall.Function.Arguments = `{"path":"/"}`
	calls := convertChatToolCalls([]chatToolCall{wireCall}, nameMap)
	require.Len(t, calls, 1)
	require.Equal(t, "filesystem.list_directory", calls[0].Function.Name)
}
