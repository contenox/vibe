package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// cache_control must land exactly on the last tool definition and the system block, nowhere else; usage normalizes to PromptTokens = input + cache reads/writes.
func TestUnit_AnthropicCache_WirePlacementAndUsage(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":900,"cache_creation_input_tokens":90}}`))
	}))
	defer srv.Close()

	client := &anthropicChatClient{anthropicClient: anthropicClient{
		baseURL:    srv.URL,
		apiKey:     "k",
		modelName:  "claude-haiku-4-5",
		httpClient: srv.Client(),
		tracker:    libtracker.NoopTracker{},
	}}

	res, err := client.Chat(context.Background(),
		[]modelrepo.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
		modelrepo.WithTools(modelrepo.Tool{Type: "function", Function: &modelrepo.FunctionTool{Name: "fs.read"}}),
		modelrepo.WithCacheHints(modelrepo.CacheHints{StableSystem: true, StableTools: true}),
	)
	require.NoError(t, err)

	var wire map[string]any
	require.NoError(t, json.Unmarshal(captured, &wire))

	tools := wire["tools"].([]any)
	lastTool := tools[len(tools)-1].(map[string]any)
	cc, ok := lastTool["cache_control"].(map[string]any)
	require.True(t, ok, "last tool must carry cache_control: %s", captured)
	require.Equal(t, "ephemeral", cc["type"])

	system := wire["system"].([]any)
	require.Len(t, system, 1)
	sysBlock := system[0].(map[string]any)
	require.Equal(t, "be terse", sysBlock["text"], "system text must be unchanged")
	_, ok = sysBlock["cache_control"].(map[string]any)
	require.True(t, ok, "system block must carry cache_control")

	require.Equal(t, 2, strings.Count(string(captured), `"cache_control"`))

	require.NotNil(t, res.Usage)
	require.Equal(t, 1000, res.Usage.PromptTokens)
	require.Equal(t, 900, res.Usage.CacheReadTokens)
	require.Equal(t, 90, res.Usage.CacheWriteTokens)
	require.Equal(t, 4, res.Usage.CompletionTokens)
}

// Without hints the request has no cache metadata and keeps the plain string system field.
func TestUnit_AnthropicCache_HintlessRequestUnchanged(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	client := &anthropicChatClient{anthropicClient: anthropicClient{
		baseURL:    srv.URL,
		apiKey:     "k",
		modelName:  "claude-haiku-4-5",
		httpClient: srv.Client(),
		tracker:    libtracker.NoopTracker{},
	}}

	res, err := client.Chat(context.Background(),
		[]modelrepo.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
		modelrepo.WithTools(modelrepo.Tool{Type: "function", Function: &modelrepo.FunctionTool{Name: "fs.read"}}),
	)
	require.NoError(t, err)
	require.NotContains(t, string(captured), "cache_control")

	var wire map[string]any
	require.NoError(t, json.Unmarshal(captured, &wire))
	require.Equal(t, "be terse", wire["system"], "hint-less system stays the plain string form")
	require.Nil(t, res.Usage, "no usage in the response means none on the result")
}
