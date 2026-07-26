package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_OpenRouterCatalog_PreservesTopProviderMaxCompletionTokens(t *testing.T) {
	m := orModel{ID: "deepseek/deepseek-chat-v3-5"}
	m.Architecture.Modality = "text->text"
	m.ContextLength = 128000
	m.TopProvider.ContextLength = 64000
	m.TopProvider.MaxCompletionTokens = 8192

	observed, ok := toObservedModel(m)
	require.True(t, ok)
	require.Equal(t, "deepseek/deepseek-chat-v3-5", observed.Name)
	require.Equal(t, 64000, observed.ContextLength)
	require.Equal(t, 8192, observed.MaxOutputTokens)
	require.True(t, observed.CanChat)
	require.False(t, observed.CanVision, "text->text model must not claim vision")
}

// TestUnit_OpenRouterCatalog_DetectsVisionFromModality asserts CanVision is
// read from the provider's own architecture metadata — the input_modalities
// list when present, else the legacy input->output modality string.
func TestUnit_OpenRouterCatalog_DetectsVisionFromModality(t *testing.T) {
	// Modern signal: input_modalities contains "image".
	m := orModel{ID: "openai/gpt-4o"}
	m.Architecture.Modality = "text+image->text"
	m.Architecture.InputModalities = []string{"text", "image"}
	observed, ok := toObservedModel(m)
	require.True(t, ok)
	require.True(t, observed.CanChat)
	require.True(t, observed.CanVision)

	// Fallback: legacy modality string with image on the input side.
	m2 := orModel{ID: "some/vlm"}
	m2.Architecture.Modality = "text+image->text"
	observed2, ok := toObservedModel(m2)
	require.True(t, ok)
	require.True(t, observed2.CanVision)

	// Image only on the output side is generation, not input — no vision.
	m3 := orModel{ID: "some/imagegen"}
	m3.Architecture.Modality = "text->text+image"
	observed3, ok := toObservedModel(m3)
	require.True(t, ok)
	require.False(t, observed3.CanVision)
}

func TestUnit_OpenRouterChat_ClampsMaxTokens(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	p := newOpenRouterProvider("secret-key", "deepseek/deepseek-chat-v3-5", srv.URL,
		modelrepo.CapabilityConfig{CanChat: true, MaxOutputTokens: 2048}, srv.Client(), nil)
	chat, err := p.GetChatConnection(context.Background(), "")
	require.NoError(t, err)

	res, err := chat.Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}}, modelrepo.WithMaxTokens(4096))
	require.NoError(t, err)

	require.True(t, strings.HasSuffix(gotPath, "/chat/completions"), "path was %q", gotPath)
	require.Equal(t, "Bearer secret-key", gotAuth)
	require.Equal(t, float64(2048), gotBody["max_tokens"])
	require.Equal(t, "ok", res.Message.Content)
}
