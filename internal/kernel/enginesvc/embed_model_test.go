package enginesvc

// resolveEmbeddingModel decides which model produces embeddings. Before the
// workspace-index prerequisite fix, llmrepo.Config.DefaultEmbeddingModel was set
// to the CHAT model unconditionally — correct only where a provider's chat model
// also embeds. These tests pin the resolution order and the never-fail rule.

import (
	"context"
	"sync"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// recSpan captures one tracked span. The fallback signal used to be a
// slog.Warn, which no test could see; recording the operation/subject pair, the
// kvArgs and the change payload is what makes "it degrades, loudly" assertable
// rather than aspirational.
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

// An unset embedding provider is the NORMAL single-backend case (one provider
// serves both models) and falls back silently; only an unset MODEL is worth a
// report, because that is the case where the chat model gets used to embed.
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

// With nothing configured anywhere, resolution must still produce a usable
// model: retrieval is optional and an unset embedding model can never fail the
// engine build. The honesty lives in the tracker report and the doctor issue.
func TestUnit_ResolveEmbeddingModel_FallsBackToChatModel(t *testing.T) {
	ctx, store := runtimetypes.SetupStore(t)

	tracker := &recTracker{}
	got := resolveEmbeddingModel(ctx, store, Config{
		DefaultModel:    "qwen2.5:7b",
		DefaultProvider: "ollama",
	}, tracker)
	require.Equal(t, "qwen2.5:7b", got.Name)
	require.Equal(t, "ollama", got.Provider)

	// The chat model silently doing the embedding is the failure mode this whole
	// resolver exists to make visible, so assert the span's full contents: an
	// operator reading it has to learn which model was substituted, under which
	// provider, and what to type to fix it.
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
	// An empty Config has no chat model to fall back TO, so the fallback is
	// vacuous — but it is still the unconfigured path, and staying silent here
	// would hide the emptiest configuration of all.
	require.NotNil(t, tracker.find("resolve", "embedding_model"),
		"an unset embedding model is reported even when the fallback resolves to nothing")
}

// A nil tracker is a legitimate caller state (Config.Tracker is optional), and
// must degrade to the Noop rather than panic on the fallback path — the path
// that is by definition reached when the caller configured the least.
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
