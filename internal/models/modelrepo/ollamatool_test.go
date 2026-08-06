package modelrepo_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/ollama"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func TestSystem_Ollama_Tools(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ollama system test: starts a container and pulls multi-GB models")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := t.Context()
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, "latest")
	require.NoError(t, err)
	defer cleanup()

	toolModel := "qwen3:4b"
	t.Logf("Pulling tool-capable model: %s", toolModel)
	err = pullModel(t, uri, toolModel)
	require.NoError(t, err, "failed to pull tool model %s", toolModel)
	err = waitForModelReady(t, uri, toolModel)
	require.NoError(t, err)

	t.Run("ToolSupport", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(toolModel, []string{uri}, http.DefaultClient, caps, "", nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		tools := []modelrepo.Tool{
			{
				Type: "function",
				Function: &modelrepo.FunctionTool{
					Name:        "get_weather",
					Description: "Get the current weather in a location",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{
								"type":        "string",
								"description": "The city and state, e.g. San Francisco, CA",
							},
							"unit": map[string]interface{}{
								"type": "string",
								"enum": []string{"celsius", "fahrenheit"},
							},
						},
						"required": []string{"location"},
					},
				},
			},
		}

		messages := []modelrepo.Message{
			{
				Role: "system",
				Content: "You are a helpful assistant with access to tools. " +
					"Use the get_weather tool when asked about weather.",
			},
			{
				Role:    "user",
				Content: "What's the weather like in Paris?",
			},
		}

		resp, err := chatClient.Chat(ctx, messages, modelrepo.WithTools(tools...))

		require.NoError(t, err)
		assert.Equal(t, "assistant", resp.Message.Role)
		assertResponseTextOrToolCalls(t, resp)

		t.Logf("Response: %s", resp.Message.Content)
		if len(resp.ToolCalls) > 0 {
			t.Logf("Tool calls: %d", len(resp.ToolCalls))
			for i, toolCall := range resp.ToolCalls {
				t.Logf("Tool call %d: %s with args: %s", i, toolCall.Function.Name, toolCall.Function.Arguments)

				assert.Equal(t, "function", toolCall.Type)
				assert.Equal(t, "get_weather", toolCall.Function.Name)

				var args map[string]interface{}
				err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args)
				assert.NoError(t, err)
				assert.Contains(t, args, "location")
			}
		}
	})

	t.Run("SingleTool", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(toolModel, []string{uri}, http.DefaultClient, caps, "", nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		tool := modelrepo.Tool{
			Type: "function",
			Function: &modelrepo.FunctionTool{
				Name:        "get_time",
				Description: "Get the current time in a timezone",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"timezone": map[string]interface{}{
							"type":        "string",
							"description": "The timezone, e.g. America/New_York",
						},
					},
					"required": []string{"timezone"},
				},
			},
		}

		messages := []modelrepo.Message{
			{
				Role: "system",
				Content: "You are a helpful assistant with access to tools. " +
					"Use the get_time tool when asked about time.",
			},
			{
				Role:    "user",
				Content: "What time is it in Tokyo?",
			},
		}

		resp, err := chatClient.Chat(ctx, messages, modelrepo.WithTool(tool))
		require.NoError(t, err)
		assert.Equal(t, "assistant", resp.Message.Role)
		assertResponseTextOrToolCalls(t, resp)

		t.Logf("Response with single tool: %s", resp.Message.Content)
		if len(resp.ToolCalls) > 0 {
			call := resp.ToolCalls[0]
			assert.Equal(t, "function", call.Type)
			assert.Equal(t, "get_time", call.Function.Name)
			var args map[string]interface{}
			err := json.Unmarshal([]byte(call.Function.Arguments), &args)
			assert.NoError(t, err)
			assert.Contains(t, args, "timezone")
		}
	})

	// Tool calls arriving over NDJSON exercise ToolCallFunctionArguments'
	// decode-then-remarshal path per frame, which the non-streaming tests
	// above never reach.
	t.Run("StreamedToolCalls", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
			CanStream:     true,
		}
		provider := ollama.NewOllamaProvider(toolModel, []string{uri}, http.DefaultClient, caps, "", nil)

		streamClient, err := provider.GetStreamConnection(ctx, uri)
		require.NoError(t, err)

		tool := modelrepo.Tool{
			Type: "function",
			Function: &modelrepo.FunctionTool{
				Name:        "get_weather",
				Description: "Get the current weather in a location",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"location": map[string]interface{}{
							"type":        "string",
							"description": "The city and state, e.g. San Francisco, CA",
						},
					},
					"required": []string{"location"},
				},
			},
		}

		messages := []modelrepo.Message{
			{
				Role: "system",
				Content: "You are a helpful assistant with access to tools. " +
					"Use the get_weather tool when asked about weather.",
			},
			{Role: "user", Content: "What's the weather like in Paris?"},
		}

		ch, err := streamClient.Stream(ctx, messages, modelrepo.WithTool(tool))
		require.NoError(t, err)

		var (
			deltas   []*modelrepo.ToolCallDelta
			content  string
			terminal *modelrepo.StreamTerminal
		)
		for parcel := range ch {
			require.NoError(t, parcel.Error)
			switch {
			case parcel.Terminal != nil:
				terminal = parcel.Terminal
			case parcel.ToolCall != nil:
				deltas = append(deltas, parcel.ToolCall)
			case parcel.Data != "":
				content += parcel.Data
			}
		}
		require.NotNil(t, terminal, "stream ended without a terminal parcel")

		if len(deltas) == 0 {
			require.NotEmpty(t, content, "stream produced neither tool calls nor content")
			t.Skip("model answered without calling a tool")
		}
		for i, delta := range deltas {
			assert.Equal(t, "function", delta.Type)
			assert.Equal(t, i, delta.Index, "tool call deltas must be sequentially indexed")
			assert.Equal(t, "get_weather", delta.Name)

			var args map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(delta.ArgsFragment), &args),
				"streamed tool arguments are not valid JSON: %s", delta.ArgsFragment)
			assert.Contains(t, args, "location")
		}
	})
}

func assertResponseTextOrToolCalls(t *testing.T, resp modelrepo.ChatResult) {
	t.Helper()
	if resp.Message.Content == "" {
		require.NotEmpty(t, resp.ToolCalls, "ollama may return empty assistant content when the model emits tool calls")
	}
}
