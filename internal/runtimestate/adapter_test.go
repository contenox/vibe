package runtimestate_test

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/vibe/internal/runtimestate"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/contenox/vibe/statetype"
	"github.com/stretchr/testify/require"
)

func TestUnit_ModelProviderAdapter_SetsCorrectModelCapabilities(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	chatModelName := "llama3:latest"
	embedModelName := "granite-embedding:30m"
	unknownModelName := "some-random-model:v1"
	backendID := "backend-test"
	backendURL := "http://host:1234"

	runtime := map[string]statetype.BackendRuntimeState{
		backendID: {
			ID:      backendID,
			Name:    "Test Backend",
			Backend: runtimetypes.Backend{ID: backendID, Name: "Ollama", Type: "ollama", BaseURL: backendURL},
			PulledModels: []statetype.ModelPullStatus{
				{
					Name:          chatModelName,
					Model:         chatModelName,
					ModifiedAt:    now,
					CanChat:       true,
					CanEmbed:      true,
					CanPrompt:     true,
					CanStream:     true,
					ContextLength: 4096,
				},
				{
					Name:          embedModelName,
					Model:         embedModelName,
					ModifiedAt:    now,
					CanEmbed:      true,
					ContextLength: 512,
				},
				{
					Name:       unknownModelName,
					Model:      unknownModelName,
					ModifiedAt: now,
				},
			},
		},
	}

	// 2. Get the adapter function
	adapterFunc := runtimestate.LocalProviderAdapter(ctx, nil, runtime)

	// 3. Get the providers
	providers, err := adapterFunc(ctx, "ollama")
	require.NoError(t, err)
	require.Len(t, providers, 3, "Should create one provider per model")

	// 4. Verify capabilities
	foundChat := false
	foundEmbed := false
	foundUnknown := false

	for _, p := range providers {
		switch p.ModelName() {
		case chatModelName:
			foundChat = true
			require.True(t, p.CanChat(), "Provider for %s should support chat", chatModelName)
			require.True(t, p.CanEmbed(), "Provider for %s should support embed", chatModelName)
		case embedModelName:
			foundEmbed = true
			require.True(t, p.CanEmbed(), "Provider for %s should support embed", embedModelName)
			require.False(t, p.CanChat(), "Provider for %s should NOT support chat", embedModelName)
		case unknownModelName:
			foundUnknown = true
		}
	}

	require.True(t, foundChat, "Provider for chat model not found")
	require.True(t, foundEmbed, "Provider for embed model not found")
	require.True(t, foundUnknown, "Provider for unknown model not found")
}

func TestUnit_ModelProviderAdapter_PropagatesCapabilitiesCorrectly(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	backendID := "backend-test"
	backendURL := "http://host:1234"

	// Define models with specific capabilities
	runtime := map[string]statetype.BackendRuntimeState{
		backendID: {
			ID:      backendID,
			Name:    "Test Backend",
			Backend: runtimetypes.Backend{ID: backendID, Name: "Ollama", Type: "ollama", BaseURL: backendURL},
			PulledModels: []statetype.ModelPullStatus{
				{
					Name:          "chat-model",
					Model:         "chat-model",
					ModifiedAt:    now,
					CanChat:       true,
					CanEmbed:      false,
					CanPrompt:     true,
					CanStream:     true,
					ContextLength: 4096,
				},
				{
					Name:          "embed-model",
					Model:         "embed-model",
					ModifiedAt:    now,
					CanChat:       false,
					CanEmbed:      true,
					CanPrompt:     false,
					CanStream:     false,
					ContextLength: 512,
				},
				{
					Name:          "full-model",
					Model:         "full-model",
					ModifiedAt:    now,
					CanChat:       true,
					CanEmbed:      true,
					CanPrompt:     true,
					CanStream:     true,
					ContextLength: 8192,
				},
			},
		},
	}

	adapterFunc := runtimestate.LocalProviderAdapter(ctx, nil, runtime)

	providers, err := adapterFunc(ctx, "ollama")
	require.NoError(t, err, "should not return error")
	require.Len(t, providers, 3, "should create one provider per model")

	// Verify capabilities
	for _, p := range providers {
		switch p.ModelName() {
		case "chat-model":
			require.True(t, p.CanChat(), "chat-model should support chat")
			require.False(t, p.CanEmbed(), "chat-model should not support embedding")
			require.True(t, p.CanPrompt(), "chat-model should support prompting")
			require.True(t, p.CanStream(), "chat-model should support streaming")
			require.Equal(t, 4096, p.GetContextLength(), "chat-model context length mismatch")

		case "embed-model":
			require.False(t, p.CanChat(), "embed-model should not support chat")
			require.True(t, p.CanEmbed(), "embed-model should support embedding")
			require.False(t, p.CanPrompt(), "embed-model should not support prompting")
			require.False(t, p.CanStream(), "embed-model should not support streaming")
			require.Equal(t, 512, p.GetContextLength(), "embed-model context length mismatch")

		case "full-model":
			require.True(t, p.CanChat(), "full-model should support chat")
			require.True(t, p.CanEmbed(), "full-model should support embedding")
			require.True(t, p.CanPrompt(), "full-model should support prompting")
			require.True(t, p.CanStream(), "full-model should support streaming")
			require.Equal(t, 8192, p.GetContextLength(), "full-model context length mismatch")
		}
	}
}

func TestUnit_ModelProviderAdapter_HandlesMissingCapabilities(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	backendID := "backend-test"
	backendURL := "http://host:1234"

	// Define model with partial capabilities
	runtime := map[string]statetype.BackendRuntimeState{
		backendID: {
			ID:      backendID,
			Name:    "Test Backend",
			Backend: runtimetypes.Backend{ID: backendID, Name: "Ollama", Type: "ollama", BaseURL: backendURL},
			PulledModels: []statetype.ModelPullStatus{
				{
					Name:       "partial-model",
					Model:      "partial-model",
					ModifiedAt: now,
					// Missing capability fields
				},
			},
		},
	}

	adapterFunc := runtimestate.LocalProviderAdapter(ctx, nil, runtime)

	providers, err := adapterFunc(ctx, "ollama")
	require.NoError(t, err, "should not return error")
	require.Len(t, providers, 1, "should create one provider")

	// Verify zero-value capabilities
	p := providers[0]
	require.False(t, p.CanChat(), "should default to no chat support")
	require.False(t, p.CanEmbed(), "should default to no embedding support")
	require.False(t, p.CanPrompt(), "should default to no prompt support")
	require.False(t, p.CanStream(), "should default to no streaming support")
	require.Equal(t, 0, p.GetContextLength(), "should default to zero context length")
}
