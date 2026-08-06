package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/require"
)

// captureRequestBody decodes the request body into dest and replies with responseJSON.
func captureRequestBody(t *testing.T, dest *map[string]any, responseJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, dest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseJSON))
	}))
}

func newGPT5ChatClient(t *testing.T, serverURL string) *OpenAIChatClient {
	t.Helper()
	return &OpenAIChatClient{
		openAIClient: openAIClient{
			baseURL:       serverURL,
			apiKey:        "key",
			httpClient:    http.DefaultClient,
			modelName:     "gpt-5",
			supportsThink: true,
			tracker:       libtracker.NoopTracker{},
		},
	}
}

const responsesOKBody = `{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"reasoning":{}}`

// An image attachment must reach the Responses API as an input_image part with an inline base64 data URI, alongside input_text.
func TestUnit_GPT5Chat_SerializesImageInput(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := captureRequestBody(t, &got, responsesOKBody)
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	_, err := client.Chat(context.Background(), []modelrepo.Message{
		{Role: "user", Content: "describe this", Images: []modelrepo.ImagePart{
			{Data: pngBytes, MimeType: "image/png"},
		}},
	})
	require.NoError(t, err)

	// gpt-5 uses the Responses API: input[0].content is [input_text, input_image].
	input, ok := got["input"].([]any)
	require.True(t, ok, "input array: %#v", got)
	require.NotEmpty(t, input)
	msg := input[0].(map[string]any)
	require.Equal(t, "message", msg["type"])
	require.Equal(t, "user", msg["role"])
	parts, ok := msg["content"].([]any)
	require.True(t, ok, "content parts: %#v", msg)
	require.Len(t, parts, 2)
	require.Equal(t, "input_text", parts[0].(map[string]any)["type"])
	img := parts[1].(map[string]any)
	require.Equal(t, "input_image", img["type"])
	wantURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	require.Equal(t, wantURI, img["image_url"])
}

func TestUnit_GPT5Chat_SystemHoistedToInstructions(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := captureRequestBody(t, &got, responsesOKBody)
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	_, err := client.Chat(context.Background(), []modelrepo.Message{
		{Role: "system", Content: "you are terse"},
		{Role: "user", Content: "hi"},
	})
	require.NoError(t, err)
	require.Equal(t, "you are terse", got["instructions"], "system message must be hoisted to instructions")

	inputs := got["input"].([]any)
	for _, item := range inputs {
		m := item.(map[string]any)
		require.NotEqual(t, "system", m["role"], "system message must not appear in input[]")
	}
}

func TestUnit_GPT5Chat_ToolResultSentAsFunctionCallOutput(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := captureRequestBody(t, &got, responsesOKBody)
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	_, err := client.Chat(context.Background(), []modelrepo.Message{
		{Role: "user", Content: "list /tmp"},
		{Role: "assistant", ToolCalls: []modelrepo.ToolCall{
			{ID: "call_1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "fs_list", Arguments: `{"path":"/tmp"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: `["a","b"]`},
	})
	require.NoError(t, err)

	inputs := got["input"].([]any)
	var toolResult map[string]any
	for _, item := range inputs {
		m := item.(map[string]any)
		if m["type"] == "function_call_output" {
			toolResult = m
		}
	}
	require.NotNil(t, toolResult, "must have a function_call_output item")
	require.Equal(t, "call_1", toolResult["call_id"])
	require.Equal(t, `["a","b"]`, toolResult["output"])
	require.Empty(t, toolResult["role"], "function_call_output must not have a role field")
}

func TestUnit_GPT5Chat_AssistantToolCallSentAsFunctionCallItem(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := captureRequestBody(t, &got, responsesOKBody)
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	_, err := client.Chat(context.Background(), []modelrepo.Message{
		{Role: "user", Content: "list /tmp"},
		{Role: "assistant", ToolCalls: []modelrepo.ToolCall{
			{ID: "call_1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "fs_list", Arguments: `{"path":"/tmp"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: `["a"]`},
	})
	require.NoError(t, err)

	inputs := got["input"].([]any)
	var fcItem map[string]any
	for _, item := range inputs {
		m := item.(map[string]any)
		if m["type"] == "function_call" {
			fcItem = m
		}
	}
	require.NotNil(t, fcItem, "assistant tool call must produce a function_call item")
	require.Equal(t, "call_1", fcItem["call_id"])
	require.Equal(t, "fs_list", fcItem["name"])
}

func TestUnit_GPT5Chat_ToolUsesResponsesFunctionShape(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := captureRequestBody(t, &got, responsesOKBody)
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	_, err := client.Chat(
		context.Background(),
		[]modelrepo.Message{{Role: "user", Content: "read README"}},
		modelrepo.WithTools(modelrepo.Tool{
			Type: "function",
			Function: &modelrepo.FunctionTool{
				Name:        "local_fs.read_file",
				Description: "Read a local file",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []any{"path"},
				},
			},
		}),
	)
	require.NoError(t, err)

	tools := got["tools"].([]any)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	require.Equal(t, "function", tool["type"])
	require.Equal(t, "local_fs_read_file", tool["name"])
	require.Equal(t, "Read a local file", tool["description"])
	require.Equal(t, false, tool["strict"])
	_, hasChatCompletionsWrapper := tool["function"]
	require.False(t, hasChatCompletionsWrapper, "Responses API function tools must not use the chat-completions function wrapper")

	parameters := tool["parameters"].(map[string]any)
	require.Equal(t, "object", parameters["type"])
	require.Contains(t, parameters["properties"], "path")
	require.Equal(t, []any{"path"}, parameters["required"])
}

func TestUnit_GPT5Chat_ToolWithoutParametersSendsEmptyParametersObject(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := captureRequestBody(t, &got, responsesOKBody)
	defer srv.Close()

	client := newGPT5ChatClient(t, srv.URL)
	_, err := client.Chat(
		context.Background(),
		[]modelrepo.Message{{Role: "user", Content: "run noop"}},
		modelrepo.WithTools(modelrepo.Tool{
			Type:     "function",
			Function: &modelrepo.FunctionTool{Name: "noop"},
		}),
	)
	require.NoError(t, err)

	tools := got["tools"].([]any)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	require.Equal(t, "noop", tool["name"])
	require.Equal(t, false, tool["strict"])
	parameters := tool["parameters"].(map[string]any)
	require.Empty(t, parameters)
}

func TestUnit_GPT5Chat_VLLMChatCompletionsUnaffected(t *testing.T) {
	t.Parallel()
	// A vLLM model (non gpt-5 prefix) must go to /chat/completions, not /responses.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	client := &OpenAIChatClient{
		openAIClient: openAIClient{
			baseURL:    srv.URL,
			apiKey:     "key",
			httpClient: srv.Client(),
			modelName:  "meta-llama-3-8b-instruct", // vLLM model
			tracker:    libtracker.NoopTracker{},
		},
	}
	_, err := client.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "ping"}})
	require.NoError(t, err)
	require.Equal(t, "/chat/completions", gotPath, "vLLM models must use /chat/completions")
}

func TestUnit_OpenAIChatCompletionResponseReasoningContent(t *testing.T) {
	t.Parallel()
	const raw = `{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Answer.",
      "reasoning_content": "Internal steps here."
    },
    "finish_reason": "stop"
  }]
}`
	var resp openAIChatCompletionResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: %d", len(resp.Choices))
	}
	m := resp.Choices[0].Message
	if m.Content != "Answer." || m.ReasoningContent != "Internal steps here." {
		t.Fatalf("message: content=%q reasoning_content=%q", m.Content, m.ReasoningContent)
	}
}
