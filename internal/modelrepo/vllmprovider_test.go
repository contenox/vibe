package modelrepo_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/internal/modelrepo/vllm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVLLM_Smoke tests the minimal functionality of the vLLM provider
func TestSystem_VLLM_Smoke(t *testing.T) {
	ctx := context.Background()

	// Use tiny model for CPU testing
	model := "HuggingFaceTB/SmolLM2-360M-Instruct"
	tag := "latest"

	t.Logf("Setting up vLLM container with model: %s", model)

	// Set up vLLM instance (shared across all subtests)
	apiBase, _, cleanup, err := modelrepo.SetupVLLMLocalInstance(ctx, model, tag, "none")
	require.NoError(t, err, "failed to setup vLLM instance")
	defer cleanup()

	// Define test cases that reuse the same container setup
	tests := []struct {
		name     string
		caps     modelrepo.CapabilityConfig
		testFunc func(t *testing.T, provider modelrepo.Provider, apiBase string)
	}{
		{
			name: "basic_chat",
			caps: modelrepo.CapabilityConfig{
				ContextLength: 512,
				CanChat:       true,
			},
			testFunc: func(t *testing.T, provider modelrepo.Provider, apiBase string) {
				// Verify basic metadata
				assert.Equal(t, model, provider.ModelName())
				assert.Equal(t, "vllm:"+model, provider.GetID())
				assert.Equal(t, "vllm", provider.GetType())
				assert.Equal(t, 512, provider.GetContextLength())
				assert.True(t, provider.CanChat())
				assert.False(t, provider.CanEmbed())
				assert.False(t, provider.CanStream())
				assert.False(t, provider.CanPrompt())

				// Get chat client
				chatClient, err := provider.GetChatConnection(ctx, apiBase)
				require.NoError(t, err, "failed to get chat connection")

				// Test a minimal conversation
				messages := []modelrepo.Message{
					{Role: "user", Content: "Hello"},
				}

				ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
				defer cancel()

				t.Log("Sending chat request to vLLM...")
				start := time.Now()
				resp, err := chatClient.Chat(ctx, messages)
				elapsed := time.Since(start)

				require.NoError(t, err, "failed to get chat response")
				assert.NotEmpty(t, resp.Message.Content, "response content should not be empty")
				assert.Equal(t, "assistant", resp.Message.Role, "response role should be assistant")

				t.Logf("Response received in %v: %q", elapsed, resp.Message.Content)
			},
		},
		{
			name: "chat_with_options",
			caps: modelrepo.CapabilityConfig{
				ContextLength: 512,
				CanChat:       true,
			},
			testFunc: func(t *testing.T, provider modelrepo.Provider, apiBase string) {
				chatClient, err := provider.GetChatConnection(ctx, apiBase)
				require.NoError(t, err, "failed to get chat connection")

				// More directive prompt to force counting behavior
				messages := []modelrepo.Message{
					{Role: "user", Content: "Count from 1 to 5. Each number on its own line. No additional text."},
				}

				// Test with custom temperature and max tokens
				ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
				defer cancel()

				t.Log("Testing chat with custom options...")
				resp, err := chatClient.Chat(ctx, messages,
					modelrepo.WithTemperature(0.1), // Very deterministic
					modelrepo.WithMaxTokens(30),
				)
				require.NoError(t, err, "failed to get chat response with options")
				assert.NotEmpty(t, resp.Message.Content, "response should not be empty")

				// More flexible validation - check for presence of numbers
				content := strings.ToLower(resp.Message.Content)
				t.Logf("Received response: %q", content)

				// Check if response contains all required numbers
				hasOne := strings.Contains(content, "1")
				hasTwo := strings.Contains(content, "2")
				hasThree := strings.Contains(content, "3")
				hasFour := strings.Contains(content, "4")
				hasFive := strings.Contains(content, "5")

				assert.True(t, hasOne && hasTwo && hasThree && hasFour && hasFive,
					"response should contain numbers 1-5 (found 1:%v, 2:%v, 3:%v, 4:%v, 5:%v): %q",
					hasOne, hasTwo, hasThree, hasFour, hasFive, content)
			},
		},
		{
			name: "prompt_execution",
			caps: modelrepo.CapabilityConfig{
				ContextLength: 512,
				CanPrompt:     true,
			},
			testFunc: func(t *testing.T, provider modelrepo.Provider, apiBase string) {
				// Verify metadata for prompt capability
				assert.True(t, provider.CanPrompt())
				assert.False(t, provider.CanChat())

				promptClient, err := provider.GetPromptConnection(ctx, apiBase)
				require.NoError(t, err, "failed to get prompt connection")

				system := "You are a helpful assistant that answers concisely."
				promptText := "What is the capital of France? Answer with just the city name."

				ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
				defer cancel()

				t.Log("Testing prompt execution...")
				start := time.Now()
				resp, err := promptClient.Prompt(ctx, system, 0.7, promptText)
				elapsed := time.Since(start)

				require.NoError(t, err, "failed to execute prompt")
				assert.NotEmpty(t, resp, "prompt response should not be empty")
				t.Logf("Prompt response in %v: %q", elapsed, resp)

				// Basic validation - should contain "Paris" (case-insensitive)
				assert.True(t, strings.Contains(strings.ToLower(resp), "paris"),
					"response should mention Paris: %q", resp)
			},
		},
		{
			name: "streaming_response",
			caps: modelrepo.CapabilityConfig{
				ContextLength: 512,
				CanStream:     true,
			},
			testFunc: func(t *testing.T, provider modelrepo.Provider, apiBase string) {
				// Verify metadata for streaming capability
				assert.True(t, provider.CanStream())
				assert.False(t, provider.CanChat())

				streamClient, err := provider.GetStreamConnection(ctx, apiBase)
				require.NoError(t, err, "failed to get stream connection")

				// Simpler prompt that's less likely to cause issues
				prompt := "Say 'Hello' and stop."

				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()

				t.Log("Testing streaming response...")
				start := time.Now()
				stream, err := streamClient.Stream(ctx, prompt)
				require.NoError(t, err, "failed to start stream")

				t.Logf("Stream setup took %v", time.Since(start))

				// Process streamed response with better error diagnostics
				var fullResponse string
				var parcelCount int
				var firstError error

				for parcel := range stream {
					if parcel.Error != nil {
						// Capture first error but keep processing
						if firstError == nil {
							firstError = parcel.Error
							t.Logf("First stream error encountered: %v", parcel.Error)
						}
						continue
					}

					if parcel.Data != "" {
						fullResponse += parcel.Data
						parcelCount++
					}
				}

				elapsed := time.Since(start)

				// If we got an error but also some data, log it as a warning
				if firstError != nil {
					t.Logf("Stream completed with partial data due to error: %v", firstError)
					// Only fail if we got no data at all
					if fullResponse == "" {
						require.Fail(t, "streaming failed with no data", "error: %v", firstError)
					}
				}

				assert.NotEmpty(t, fullResponse, "streamed response should not be empty")
				assert.Greater(t, parcelCount, 0, "should receive at least one stream parcel")
				t.Logf("Stream completed in %v, %d parcels: %q", elapsed, parcelCount, fullResponse)

				// Basic validation of streaming content
				assert.True(t, strings.Contains(strings.ToLower(fullResponse), "hello"),
					"streamed response should contain 'hello': %q", fullResponse)
			},
		},
		{
			name: "chat_with_seed_reproducibility",
			caps: modelrepo.CapabilityConfig{
				ContextLength: 512,
				CanChat:       true,
			},
			testFunc: func(t *testing.T, provider modelrepo.Provider, apiBase string) {
				chatClient, err := provider.GetChatConnection(ctx, apiBase)
				require.NoError(t, err, "failed to get chat connection")

				// Simple prompt where we can verify determinism
				messages := []modelrepo.Message{
					{Role: "system", Content: "You are a helpful assistant. Answer briefly."},
					{Role: "user", Content: "How many moons does Earth have?"},
				}

				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()

				// First request with seed 123
				resp1, err := chatClient.Chat(ctx, messages,
					modelrepo.WithSeed(123),
					modelrepo.WithTemperature(0.7), // Keep some randomness but controlled
					modelrepo.WithMaxTokens(10),
				)
				require.NoError(t, err, "failed to get first response")
				require.NotEmpty(t, resp1.Message.Content, "first response should not be empty")

				// Second request with SAME seed
				resp2, err := chatClient.Chat(ctx, messages,
					modelrepo.WithSeed(123),
					modelrepo.WithTemperature(0.7),
					modelrepo.WithMaxTokens(10),
				)
				require.NoError(t, err, "failed to get second response")
				require.NotEmpty(t, resp2.Message.Content, "second response should not be empty")

				// Critical check: same seed = same output
				assert.Equal(t, resp1.Message.Content, resp2.Message.Content,
					"Responses with identical seed should be identical")

				t.Logf("Verified deterministic output with seed 123: %q", resp1.Message.Content)

				// Third request with DIFFERENT seed
				resp3, err := chatClient.Chat(ctx, messages,
					modelrepo.WithSeed(456),
					modelrepo.WithTemperature(0.7),
					modelrepo.WithMaxTokens(10),
				)
				require.NoError(t, err, "failed to get third response")
				require.NotEmpty(t, resp3.Message.Content, "third response should not be empty")

				// Sanity check: different seed should produce different output
				// (not guaranteed but highly likely - warn if same)
				if resp1.Message.Content == resp3.Message.Content {
					t.Logf("WARNING: Responses with different seeds were identical. "+
						"This is unusual but can happen with very short completions. \n\n%s\n%s\n%s", resp1.Message.Content, resp2.Message.Content, resp3.Message.Content)
				} else {
					t.Logf("Confirmed different outputs with different seeds")
				}
			},
		},
	}

	// Run all test cases with shared container
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := vllm.NewVLLMProvider(
				model,
				[]string{apiBase},
				http.DefaultClient,
				tt.caps,
				"", // No auth token for local testing
				nil,
			)
			tt.testFunc(t, provider, apiBase)
		})
	}
}
