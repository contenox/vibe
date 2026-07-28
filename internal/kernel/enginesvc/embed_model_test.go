package enginesvc

// These tests pin resolveEmbeddingModel's resolution order and its
// never-fail-Build rule.

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// recSpan captures one tracked span, so a test can assert what the fallback
// reported instead of only that it happened.
type recSpan struct {
	op         string
	subject    string
	kv         []any
	changeID   string
	changeData any
	changes    int
	ended      int
}

func (s *recSpan) kvStr(key string) string {
	for i := 0; i+1 < len(s.kv); i += 2 {
		if k, ok := s.kv[i].(string); ok && k == key {
			v, _ := s.kv[i+1].(string)
			return v
		}
	}
	return ""
}

type recTracker struct {
	mu    sync.Mutex
	spans []*recSpan
}

func (rt *recTracker) Start(_ context.Context, op, subject string, kv ...any) (func(error), func(string, any), func()) {
	rt.mu.Lock()
	s := &recSpan{op: op, subject: subject, kv: append([]any(nil), kv...)}
	rt.spans = append(rt.spans, s)
	rt.mu.Unlock()
	return func(error) {},
		func(id string, data any) {
			rt.mu.Lock()
			s.changes++
			s.changeID = id
			s.changeData = data
			rt.mu.Unlock()
		},
		func() { rt.mu.Lock(); s.ended++; rt.mu.Unlock() }
}

func (rt *recTracker) find(op, subject string) *recSpan {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, s := range rt.spans {
		if s.op == op && s.subject == subject {
			return s
		}
	}
	return nil
}

func TestUnit_ResolveEmbeddingModel_PrefersExplicitConfig(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)
	require.NoError(t, clikv.SetString(ctx, store, "default-embed-model", "kv-embed"))
	require.NoError(t, clikv.SetString(ctx, store, "default-embed-provider", "kv-provider"))

	tracker := &recTracker{}
	got := resolveEmbeddingModel(ctx, store, Config{
		DefaultModel:         "chat-model",
		DefaultProvider:      "chat-provider",
		DefaultEmbedModel:    "explicit-embed",
		DefaultEmbedProvider: "explicit-provider",
	}, tracker)
	require.Equal(t, "explicit-embed", got.Name, "an explicit Config field outranks the KV key")
	require.Equal(t, "explicit-provider", got.Provider)
	require.Nil(t, tracker.find("resolve", "embedding_model"),
		"a configured embedding model is not a degradation and must report nothing")
}

func TestUnit_ResolveEmbeddingModel_ReadsConfigKeys(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)
	require.NoError(t, clikv.SetString(ctx, store, "default-embed-model", "nomic-embed-text"))
	require.NoError(t, clikv.SetString(ctx, store, "default-embed-provider", "ollama"))

	tracker := &recTracker{}
	got := resolveEmbeddingModel(ctx, store, Config{
		DefaultModel:    "qwen2.5:7b",
		DefaultProvider: "openai",
	}, tracker)
	require.Equal(t, "nomic-embed-text", got.Name)
	require.Equal(t, "ollama", got.Provider, "the embedding provider is independent of the chat provider")
	require.Nil(t, tracker.find("resolve", "embedding_model"),
		"resolution from the KV keys is the configured path, not a fallback")
}

// TestUnit_ResolveEmbeddingModel_ProviderAloneFallsBack pins that an unset
// provider alone falls back silently; only an unset model is reported.
func TestUnit_ResolveEmbeddingModel_ProviderAloneFallsBack(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)
	require.NoError(t, clikv.SetString(ctx, store, "default-embed-model", "nomic-embed-text"))

	tracker := &recTracker{}
	got := resolveEmbeddingModel(ctx, store, Config{
		DefaultModel:    "qwen2.5:7b",
		DefaultProvider: "ollama",
	}, tracker)
	require.Equal(t, "nomic-embed-text", got.Name)
	require.Equal(t, "ollama", got.Provider)
	require.Nil(t, tracker.find("resolve", "embedding_model"),
		"the provider falling back alone is silent by design; reporting it would train operators to ignore the report that matters")
}

// TestUnit_ResolveEmbeddingModel_FallsBackToChatModel pins that resolution
// still produces a usable model with nothing configured; the substitution is
// reported through the tracker, never fails Build.
func TestUnit_ResolveEmbeddingModel_FallsBackToChatModel(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)

	tracker := &recTracker{}
	got := resolveEmbeddingModel(ctx, store, Config{
		DefaultModel:    "qwen2.5:7b",
		DefaultProvider: "ollama",
	}, tracker)
	require.Equal(t, "qwen2.5:7b", got.Name)
	require.Equal(t, "ollama", got.Provider)

	// Assert the span's full contents: an operator reading it must learn
	// which model was substituted, under which provider, and the remedy.
	span := tracker.find("resolve", "embedding_model")
	require.NotNil(t, span, "substituting the chat model must be reported through the tracker")
	require.Equal(t, 1, span.changes, "the fallback must be reported exactly once")
	require.Equal(t, 1, span.ended, "the span must be closed")
	require.Equal(t, "qwen2.5:7b", span.changeID, "the report must name the model that was substituted in")
	require.Equal(t, "no embedding model configured; falling back to the chat model", span.changeData)
	require.Equal(t, "qwen2.5:7b", span.kvStr("fallback_model"))
	require.Equal(t, "ollama", span.kvStr("fallback_provider"))
	require.Contains(t, span.kvStr("hint"), "contenox config set default-embed-model",
		"the report must carry the remedy, not just the diagnosis")
}

func TestUnit_ResolveEmbeddingModel_ToleratesEmptyEverything(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)
	tracker := &recTracker{}
	got := resolveEmbeddingModel(ctx, store, Config{}, tracker)
	require.Empty(t, got.Name)
	require.Empty(t, got.Provider)
	// An empty Config has no chat model to fall back to, but it is still the
	// unconfigured path and must still be reported.
	require.NotNil(t, tracker.find("resolve", "embedding_model"),
		"an unset embedding model is reported even when the fallback resolves to nothing")
}

// TestUnit_ResolveEmbeddingModel_NilTrackerDegradesToNoop pins that a nil
// tracker (Config.Tracker is optional) degrades to the Noop rather than
// panicking on the fallback path.
func TestUnit_ResolveEmbeddingModel_NilTrackerDegradesToNoop(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)
	var nilTracker libtracker.ActivityTracker
	got := resolveEmbeddingModel(ctx, store, Config{
		DefaultModel:    "qwen2.5:7b",
		DefaultProvider: "ollama",
	}, nilTracker)
	require.Equal(t, "qwen2.5:7b", got.Name)
	require.Equal(t, "ollama", got.Provider)
}
