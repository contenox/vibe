package modelrepo_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/ollama"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

var chatModel = "smollm2:135m"

// TestSystem_Ollama exercises the Ollama provider against a live container.
func TestSystem_Ollama(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ollama system test: starts a container and pulls multi-GB models")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := t.Context()
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, "latest")
	require.NoError(t, err)
	defer cleanup()

	t.Logf("Pulling chat model: %s", chatModel)
	err = pullModel(t, uri, chatModel)
	require.NoError(t, err, "failed to pull chat model %s", chatModel)
	err = waitForModelReady(t, uri, chatModel)
	require.NoError(t, err)

	// Pull embedding model
	embedModel := "nomic-embed-text:latest"
	t.Logf("Pulling embedding model: %s", embedModel)
	err = pullModel(t, uri, embedModel)
	require.NoError(t, err, "failed to pull embedding model %s", embedModel)
	err = waitForModelReady(t, uri, embedModel)
	require.NoError(t, err)
	t.Run("Smoke", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
			CanEmbed:      true,
			CanStream:     true,
			CanPrompt:     true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		assert.Equal(t, chatModel, provider.ModelName())
		assert.Equal(t, "ollama:"+chatModel, provider.GetID())
		assert.Equal(t, "ollama", provider.GetType())
		assert.Equal(t, 2048, provider.GetContextLength())
		assert.True(t, provider.CanChat())
		assert.True(t, provider.CanEmbed())
		assert.True(t, provider.CanPrompt())
		assert.True(t, provider.CanStream())

		backends := provider.GetBackendIDs()
		assert.Equal(t, []string{uri}, backends)
	})

	t.Run("CapabilityEnforcement", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			CanChat: true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		_, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		_, err = provider.GetEmbedConnection(ctx, uri)
		assert.ErrorContains(t, err, "does not support embeddings")

		_, err = provider.GetPromptConnection(ctx, uri)
		assert.ErrorContains(t, err, "does not support prompting")
	})
	t.Run("BasicConversation", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

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
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

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
		provider := ollama.NewOllamaProvider(embedModel, []string{uri}, http.DefaultClient, caps, "", nil)

		embedClient, err := provider.GetEmbedConnection(ctx, uri)
		require.NoError(t, err)

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
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		promptClient, err := provider.GetPromptConnection(ctx, uri)
		require.NoError(t, err)

		system := "You are a Task Engine answering other Machines directly without explanation"
		prompt := "What is the capital of France?"
		resp, _, err := promptClient.Prompt(ctx, system, 0.7, prompt)
		require.NoError(t, err)
		assert.Contains(t, resp, "Paris")
		assert.NotContains(t, resp, "think")
	})

	t.Run("DeterministicOutput", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanPrompt:     true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		promptClient, err := provider.GetPromptConnection(ctx, uri)
		require.NoError(t, err)

		resp, _, err := promptClient.Prompt(ctx, "You are a calculator", 0.1, "How much is 2 + 2?")
		require.NoError(t, err)
		assert.Contains(t, resp, "4")
	})

	// Streaming is the only path that decodes NDJSON frame by frame, so it is
	// the only one that proves the per-chunk ChatResponse shape — including
	// the untagged embedded Metrics, which surface as zero usage if the
	// struct stops inlining them.
	t.Run("Streaming", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
			CanStream:     true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		streamClient, err := provider.GetStreamConnection(ctx, uri)
		require.NoError(t, err)

		messages := []modelrepo.Message{
			{Role: "system", Content: "You are a task processor talking to other machines. Answer briefly."},
			{Role: "user", Content: "What is the capital of Italy?"},
		}
		ch, err := streamClient.Stream(ctx, messages,
			modelrepo.WithTemperature(0.1),
			modelrepo.WithMaxTokens(60))
		require.NoError(t, err)

		var (
			content  strings.Builder
			chunks   int
			terminal *modelrepo.StreamTerminal
		)
		for parcel := range ch {
			require.NoError(t, parcel.Error)
			switch {
			case parcel.Terminal != nil:
				require.Nil(t, terminal, "stream emitted more than one terminal parcel")
				terminal = parcel.Terminal
			case parcel.Data != "":
				chunks++
				content.WriteString(parcel.Data)
			}
		}

		require.NotNil(t, terminal, "stream ended without a terminal parcel")
		assert.Greater(t, chunks, 1, "NDJSON framing should deliver more than one content chunk")
		assert.Contains(t, []string{"stop", "length"}, terminal.FinishReason)
		require.NotNil(t, terminal.Usage)
		assert.Positive(t, terminal.Usage.PromptTokens, "prompt_eval_count did not decode from the final frame")
		assert.Positive(t, terminal.Usage.CompletionTokens, "eval_count did not decode from the final frame")
		assert.Contains(t, strings.ToLower(content.String()), "rome")
	})

	// Duration marshals keep_alive as an ollama duration string ("10m0s").
	// Only a real server proves it parsed: residency past the server's own 5m
	// default is the observable difference.
	t.Run("KeepAliveApplied", func(t *testing.T) {
		caps := modelrepo.CapabilityConfig{
			ContextLength: 2048,
			CanChat:       true,
		}
		provider := ollama.NewOllamaProvider(chatModel, []string{uri}, http.DefaultClient, caps, "", nil)

		chatClient, err := provider.GetChatConnection(ctx, uri)
		require.NoError(t, err)

		_, err = chatClient.Chat(ctx,
			[]modelrepo.Message{{Role: "user", Content: "Say hi."}},
			modelrepo.WithMaxTokens(16))
		require.NoError(t, err)

		expiresAt, ok := runningModelExpiry(t, uri, chatModel)
		require.True(t, ok, "model %s is not reported as loaded by /api/ps", chatModel)

		residency := time.Until(expiresAt)
		assert.Greater(t, residency, 6*time.Minute,
			"keep_alive did not reach the server: residency %s is at or below the 5m server default", residency)
		assert.LessOrEqual(t, residency, 11*time.Minute,
			"residency %s exceeds the 10m window the client asks for", residency)
	})

	// The five capability strings are declared locally now that the ollama SDK
	// is gone. Only a real /api/show response proves they still match, and a
	// mismatch silently misclassifies every model.
	t.Run("CatalogCapabilityDerivation", func(t *testing.T) {
		catalog, err := modelrepo.NewCatalogProvider(
			modelrepo.BackendSpec{Type: "ollama", BaseURL: uri},
			modelrepo.WithCatalogHTTPClient(http.DefaultClient),
		)
		require.NoError(t, err)

		observed, err := catalog.ListModels(ctx)
		require.NoError(t, err)
		require.NotEmpty(t, observed, "/api/tags returned no models")

		byName := make(map[string]modelrepo.ObservedModel, len(observed))
		names := make([]string, 0, len(observed))
		for _, m := range observed {
			byName[m.Name] = m
			names = append(names, m.Name)
		}

		chat, ok := byName[chatModel]
		require.True(t, ok, "listed models %v do not include %s", names, chatModel)
		assert.True(t, chat.CanChat, "completion capability not derived for %s", chatModel)
		assert.True(t, chat.CanPrompt, "completion capability not derived for %s", chatModel)
		assert.True(t, chat.CanStream, "completion capability not derived for %s", chatModel)
		assert.False(t, chat.CanEmbed, "%s must not be classified as an embedder", chatModel)
		assert.Positive(t, chat.ContextLength, "context length not read from model_info")

		embed, ok := byName[embedModel]
		require.True(t, ok, "listed models %v do not include %s", names, embedModel)
		assert.True(t, embed.CanEmbed, "embedding capability not derived for %s", embedModel)
		assert.False(t, embed.CanChat, "%s must not be classified as a chat model", embedModel)
	})
}

// runningModelExpiry reads the residency deadline the server recorded for a
// loaded model from /api/ps.
func runningModelExpiry(t *testing.T, baseURL, model string) (time.Time, bool) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/api/ps", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, http.StatusBadRequest, "/api/ps returned %d", resp.StatusCode)

	var body struct {
		Models []struct {
			Model     string    `json:"model"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	for _, m := range body.Models {
		if m.Model == model {
			return m.ExpiresAt, true
		}
	}
	return time.Time{}, false
}

// pullModel streams POST /api/pull, logging progress. baseURL is the container
// root, e.g. "http://127.0.0.1:32768".
func pullModel(t *testing.T, baseURL, model string) error {
	t.Helper()

	body, err := json.Marshal(map[string]any{"name": model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pull %s returned %d: %s", model, resp.StatusCode, raw)
	}

	lastLogLine := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var pr struct {
			Status    string `json:"status"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(line, &pr); err != nil {
			return err
		}
		if pr.Error != "" {
			return fmt.Errorf("pull %s: %s", model, pr.Error)
		}
		if pr.Completed != 0 && pr.Total != 0 {
			progress := float64(pr.Completed) / float64(pr.Total) * 100
			logline := fmt.Sprintf("Pulling model %s: %s %f%%", model, pr.Status, progress)
			if lastLogLine != logline {
				lastLogLine = logline
				t.Log(logline)
			}
		}
	}
	return scanner.Err()
}

// waitForModelReady polls POST /api/show until the model resolves.
func waitForModelReady(t *testing.T, baseURL, model string) error {
	t.Helper()
	const maxRetries = 10
	const retryInterval = 2 * time.Second

	for i := range maxRetries {
		if err := showModel(t, baseURL, model); err == nil {
			return nil
		}
		if i < maxRetries-1 {
			time.Sleep(retryInterval)
		}
	}
	return fmt.Errorf("model %s not ready after %d retries", model, maxRetries)
}

func showModel(t *testing.T, baseURL, model string) error {
	t.Helper()

	body, err := json.Marshal(map[string]any{"name": model})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/api/show", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("show %s returned %d: %s", model, resp.StatusCode, raw)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
