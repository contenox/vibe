package modelrepo_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/internal/modelrepo/ollama"
	"github.com/ollama/ollama/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystem_Ollama_Tools(t *testing.T) {
	// Set up shared test environment
	ctx := t.Context()
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, "latest")
	require.NoError(t, err)
	defer cleanup()

	// Create shared Ollama client
	u, err := url.Parse(uri)
	require.NoError(t, err)
	ollamaClient := api.NewClient(u, http.DefaultClient)

	toolModel := "qwen3:4b"
	t.Logf("Pulling tool-capable model: %s", toolModel)
	err = pullModel(t, ollamaClient, toolModel)
	require.NoError(t, err, "failed to pull tool model %s", toolModel)
	err = waitForModelReady(t, ollamaClient, toolModel)
	require.NoError(t, err)

	t.Run("ToolSupport", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(toolModel, []string{uri}, http.DefaultClient, caps, nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		// Define a simple tool
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

		// Test conversation with tools
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
		assert.NotEmpty(t, resp.Message.Content)
		assert.Equal(t, "assistant", resp.Message.Role)

		t.Logf("Response: %s", resp.Message.Content)
		if len(resp.ToolCalls) > 0 {
			t.Logf("Tool calls: %d", len(resp.ToolCalls))
			for i, toolCall := range resp.ToolCalls {
				t.Logf("Tool call %d: %s with args: %s", i, toolCall.Function.Name, toolCall.Function.Arguments)

				// Verify the tool call structure
				assert.Equal(t, "function", toolCall.Type)
				assert.Equal(t, "get_weather", toolCall.Function.Name)

				// Parse the arguments to verify they're valid JSON
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
		provider := ollama.NewOllamaProvider(toolModel, []string{uri}, http.DefaultClient, caps, nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		// Define a single tool
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
		assert.NotEmpty(t, resp.Message.Content)
		assert.Equal(t, "assistant", resp.Message.Role)

		t.Logf("Response with single tool: %s", resp.Message.Content)
	})
}
