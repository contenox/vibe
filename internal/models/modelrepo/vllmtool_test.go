package modelrepo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/modelrepo/vllm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// focuses only on the integration flow not breaking, bigger models would be required for more proper testing
func TestSystem_VLLM_Tools(t *testing.T) {
	requireVLLMContainerTests(t)

	ctx := context.Background()

	// Qwen2.5-Instruct ships Hermes-style tool-use in its chat template, so vLLM
	// serves tool calls with --tool-call-parser hermes. The 0.5B variant is the
	// smallest ungated model that actually supports tool calling on CPU.
	// (DialoGPT/GPT-2 have no chat template and cannot do tools at all.)
	model := "Qwen/Qwen2.5-0.5B-Instruct"
	tag := "latest"

	t.Logf("Setting up vLLM container with model: %s", model)

	// Set up vLLM instance
	// deepseek_v3,granite-20b-fc,granite,hermes,internlm,jamba,llama4_pythonic,llama4_json,llama3_json,mistral,phi4_mini_json,pythonic
	apiBase, _, cleanup, err := modelrepo.SetupVLLMLocalInstance(ctx, model, tag, "hermes")
	require.NoError(t, err, "failed to setup vLLM instance")
	defer cleanup()

	t.Run("ToolSupport", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := vllm.NewVLLMProvider(
			model,
			[]string{apiBase},
			http.DefaultClient,
			caps,
			"", // No auth token for local testing
			nil,
		)

		chatClient, err := provider.GetChatConnection(ctx, apiBase)
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
		assert.Equal(t, "assistant", resp.Message.Role)
		// A tool-calling response carries tool_calls with (usually) empty content;
		// a plain answer carries content. The integration flow is healthy as long
		// as one of them is present — a small model may not always choose to call.
		assert.True(t, resp.Message.Content != "" || len(resp.ToolCalls) > 0,
			"response must contain either content or tool calls")

		t.Logf("Response content: %q", resp.Message.Content)
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
		provider := vllm.NewVLLMProvider(
			model,
			[]string{apiBase},
			http.DefaultClient,
			caps,
			"", // No auth token for local testing
			nil,
		)

		chatClient, err := provider.GetChatConnection(ctx, apiBase)
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
		assert.Equal(t, "assistant", resp.Message.Role)
		assert.True(t, resp.Message.Content != "" || len(resp.ToolCalls) > 0,
			"response must contain either content or tool calls")

		t.Logf("Response content: %q, tool calls: %d", resp.Message.Content, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			assert.Equal(t, "function", tc.Type)
			t.Logf("Tool call: %s with args: %s", tc.Function.Name, tc.Function.Arguments)
		}
	})
}
