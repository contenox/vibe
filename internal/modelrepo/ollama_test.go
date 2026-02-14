package modelrepo_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/internal/modelrepo/ollama"
	"github.com/ollama/ollama/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var chatModel = "smollm2:135m" // "tinyllama:1.1b" // "qwen3:4b" //  "tinyllama:1.1b"

// TestSystem_Ollama comprehensively tests the Ollama provider with a shared container setup
func TestSystem_Ollama(t *testing.T) {
	// Set up shared test environment
	ctx := t.Context()
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, "latest")
	require.NoError(t, err)
	defer cleanup()

	// Create shared Ollama client
	u, err := url.Parse(uri)
	require.NoError(t, err)
	ollamaClient := api.NewClient(u, http.DefaultClient)

	// Pull models needed for testing
	t.Logf("Pulling chat model: %s", chatModel)
	err = pullModel(t, ollamaClient, chatModel)
	require.NoError(t, err, "failed to pull chat model %s", chatModel)
	err = waitForModelReady(t, ollamaClient, chatModel)
	require.NoError(t, err)

	// Pull embedding model
	embedModel := "nomic-embed-text:latest"
	t.Logf("Pulling embedding model: %s", embedModel)
	err = pullModel(t, ollamaClient, embedModel)
	require.NoError(t, err, "failed to pull embedding model %s", embedModel)
	err = waitForModelReady(t, ollamaClient, embedModel)
	require.NoError(t, err)
	t.Run("Smoke", func(t *testing.T) {
		// Create provider with all capabilities enabled
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
			CanEmbed:      true,
			CanStream:     true,
			CanPrompt:     true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, nil)

		// Verify provider metadata
		assert.Equal(t, chatModel, provider.ModelName())
		assert.Equal(t, "ollama:"+chatModel, provider.GetID())
		assert.Equal(t, "ollama", provider.GetType())
		assert.Equal(t, 2048, provider.GetContextLength())
		assert.True(t, provider.CanChat())
		assert.True(t, provider.CanEmbed())
		assert.True(t, provider.CanPrompt())
		assert.True(t, provider.CanStream())

		// Verify backend configuration
		backends := provider.GetBackendIDs()
		assert.Equal(t, []string{uri}, backends)
	})

	t.Run("CapabilityEnforcement", func(t *testing.T) {
		// Create provider with only chat capability
		caps := modelrepo.CapabilityConfig{
			CanChat: true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, nil)

		// Verify chat works
		_, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		// Verify embedding fails
		_, err = provider.GetEmbedConnection(ctx, uri)
		assert.ErrorContains(t, err, "does not support embeddings")

		// Verify prompt fails
		_, err = provider.GetPromptConnection(ctx, uri)
		assert.ErrorContains(t, err, "does not support prompting")
	})
	t.Run("BasicConversation", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		// Test simple conversation
		messages := []modelrepo.Message{
			{Role: "system", Content: "You are a helpful, concise assistant."},
			{Role: "user", Content: "Hello, how are you?"},
		}
		resp, err := chatClient.Chat(ctx, messages)
		require.NoError(t, err)
		assert.NotEmpty(t, resp.Message.Content)
		assert.Equal(t, "assistant", resp.Message.Role)
		assert.NotContains(t, resp.Message.Content, "<think>")
	})

	t.Run("WithOptions", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		// Test with options
		messages := []modelrepo.Message{
			{Role: "system", Content: "You are a task processor talking to other machines. Answer briefly."},
			{Role: "user", Content: "What is the capital of Italy?"},
		}
		resp, err := chatClient.Chat(ctx, messages,

			modelrepo.WithTemperature(0.1),
			modelrepo.WithMaxTokens(60))
		require.NoError(t, err)
		assert.Contains(t, strings.ToLower(resp.Message.Content), "rome")
	})

	t.Run("BasicEmbedding", func(t *testing.T) {
		embedModel := "nomic-embed-text:latest"

		caps := modelrepo.CapabilityConfig{
			ContextLength: 8192,
			CanEmbed:      true,
		}
		provider := ollama.NewOllamaProvider(embedModel, []string{uri}, http.DefaultClient, caps, nil)

		embedClient, err := provider.GetEmbedConnection(ctx, uri)
		require.NoError(t, err)

		// Test embedding generation
		text := "The quick brown fox jumps over the lazy dog"
		embedding, err := embedClient.Embed(ctx, text)
		require.NoError(t, err)
		assert.NotEmpty(t, embedding)
		assert.Equal(t, 768, len(embedding)) // nomic-embed-text uses 768 dimensions
	})

	t.Run("BasicPrompt", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanPrompt:     true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, nil)

		promptClient, err := provider.GetPromptConnection(ctx, uri)
		require.NoError(t, err)

		// Test basic prompt
		system := "You are a Task Engine answering other Machines directly without explanation"
		prompt := "What is the capital of France?"
		resp, err := promptClient.Prompt(ctx, system, 0.7, prompt)
		require.NoError(t, err)
		assert.Contains(t, resp, "Paris")
		assert.NotContains(t, resp, "think")
	})

	t.Run("DeterministicOutput", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanPrompt:     true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, nil)

		promptClient, err := provider.GetPromptConnection(ctx, uri)
		require.NoError(t, err)

		// Test with low temperature for deterministic output
		resp, err := promptClient.Prompt(ctx, "You are a calculator", 0.1, "How much is 2 + 2?")
		require.NoError(t, err)
		assert.Contains(t, resp, "4")
	})
}

// Helper to pull a model into the Ollama instance
func pullModel(t *testing.T, client *api.Client, model string) error {
	t.Helper()
	req := &api.PullRequest{Name: model}
	lastLogLine := ""
	return client.Pull(t.Context(), req, func(pr api.ProgressResponse) error {
		if pr.Completed != 0 && pr.Total != 0 {
			progress := float64(pr.Completed) / float64(pr.Total) * 100
			logline := fmt.Sprintf("Pulling model %s: %s %f%%", model, pr.Status, progress)
			if lastLogLine != logline {
				lastLogLine = logline
				t.Log(logline)
			}
		}
		return nil
	})
}

// Helper to verify model is loaded and ready
func waitForModelReady(t *testing.T, client *api.Client, model string) error {
	t.Helper()
	const maxRetries = 10
	const retryInterval = 2 * time.Second

	for i := range maxRetries {
		_, err := client.Show(t.Context(), &api.ShowRequest{Name: model})
		if err == nil {
			return nil
		}
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	return fmt.Errorf("model %s not ready after %d retries", model, maxRetries)
}
