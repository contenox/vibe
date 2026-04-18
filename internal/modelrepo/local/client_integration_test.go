//go:build integration

package local

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/contenox/contenox/internal/modelrepo"
)

const (
	tinyModelURL  = "https://huggingface.co/Hjgugugjhuhjggg/FastThink-0.5B-Tiny-Q2_K-GGUF/resolve/main/fastthink-0.5b-tiny-q2_k.gguf"
	tinyModelName = "fastthink-0.5b-tiny-q2_k.gguf"
)

// tinyModelPath returns a cached path to the tiny test model, downloading it if necessary.
func tinyModelPath(t *testing.T) string {
	t.Helper()
	cacheDir := filepath.Join(os.TempDir(), "contenox-test-models")
	require.NoError(t, os.MkdirAll(cacheDir, 0755))
	dest := filepath.Join(cacheDir, tinyModelName)
	if _, err := os.Stat(dest); err == nil {
		return dest
	}
	t.Logf("downloading tiny test model to %s ...", dest)
	resp, err := http.Get(tinyModelURL) //nolint:gosec
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	f, err := os.Create(dest)
	require.NoError(t, err)
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	require.NoError(t, err)
	return dest
}

func userMsg(content string) modelrepo.Message {
	return modelrepo.Message{Role: "user", Content: content}
}

func TestIntegration_Local_Chat(t *testing.T) {
	path := tinyModelPath(t)
	client := &localChatClient{modelPath: path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := client.Chat(ctx, []modelrepo.Message{userMsg("Say hello in one word.")})
	require.NoError(t, err)
	assert.Equal(t, "assistant", result.Message.Role)
	assert.NotEmpty(t, result.Message.Content)
	t.Logf("Chat response: %q", result.Message.Content)
}

func TestIntegration_Local_Prompt(t *testing.T) {
	path := tinyModelPath(t)
	client := &localPromptClient{modelPath: path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := client.Prompt(ctx, "You are a helpful assistant.", 0.5, "Say hello in one word.")
	require.NoError(t, err)
	assert.NotEmpty(t, out)
	t.Logf("Prompt response: %q", out)
}

func TestIntegration_Local_Stream(t *testing.T) {
	path := tinyModelPath(t)
	client := &localStreamClient{modelPath: path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ch, err := client.Stream(ctx, []modelrepo.Message{userMsg("Say hello in one word.")})
	require.NoError(t, err)

	var sb strings.Builder
	for p := range ch {
		require.NoError(t, p.Error, "unexpected stream error parcel")
		sb.WriteString(p.Data)
	}

	assert.NotEmpty(t, sb.String())
	t.Logf("Stream response: %q", sb.String())
}

func TestIntegration_Local_Embed(t *testing.T) {
	path := tinyModelPath(t)
	client := &localEmbedClient{modelPath: path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	emb, err := client.Embed(ctx, "hello world")
	require.NoError(t, err)
	assert.NotEmpty(t, emb)

	allZero := true
	for _, v := range emb {
		if v != 0 {
			allZero = false
			break
		}
	}
	assert.False(t, allZero, "embedding vector should not be all zeros")
	t.Logf("Embedding dim=%d, first 5 values: %v", len(emb), emb[:min(5, len(emb))])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
