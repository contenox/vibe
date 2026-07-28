package llmrepo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/runtimestate"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// boundedManager is a modelManager with no runtime state, for decision-only
// tests: enforceResolutionBounds consults state only to name a backend id.
func boundedManager() *modelManager {
	return &modelManager{tracker: libtracker.NoopTracker{}}
}

func providerNamed(model string) libmodelprovider.Provider {
	return &libmodelprovider.MockProvider{Name: model, ID: model, CanChatFlag: true}
}

// A unit whose envelope forbids a model cannot use it; the refusal names the bound, what's permitted, and what was picked.
func TestUnit_ResolutionBounds_RefusesModelOutsideAllowlist(t *testing.T) {
	mm := boundedManager()
	ctx := WithResolutionBounds(context.Background(), ResolutionBounds{
		Models: []string{"gemini-2.5-flash"},
	})

	err := mm.enforceResolutionBounds(ctx, "chat", providerNamed("gpt-5"), "backend-1")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrResolutionOutOfBounds)
	require.Contains(t, err.Error(), "modelAllowlist")
	require.Contains(t, err.Error(), `"gemini-2.5-flash"`)
	require.Contains(t, err.Error(), `"gpt-5"`)
	require.Contains(t, err.Error(), "chat")
	require.Contains(t, err.Error(), "Nothing was sent")
}

// The allowed case must pass cleanly, including whitespace/case slack from a hand-edited envelope.
func TestUnit_ResolutionBounds_AdmitsListedModel(t *testing.T) {
	mm := boundedManager()
	ctx := WithResolutionBounds(context.Background(), ResolutionBounds{
		Models: []string{"  Gemini-2.5-Flash  ", "gpt-5"},
	})
	require.NoError(t, mm.enforceResolutionBounds(ctx, "chat", providerNamed("gemini-2.5-flash"), "backend-1"))
	require.NoError(t, mm.enforceResolutionBounds(ctx, "stream", providerNamed("gpt-5"), "backend-1"))
}

// Matching must not use the resolver's NormalizeModelName, which strips version tags/punctuation and would silently widen a security boundary.
func TestUnit_ResolutionBounds_DoesNotWidenByNormalization(t *testing.T) {
	mm := boundedManager()
	ctx := WithResolutionBounds(context.Background(), ResolutionBounds{
		Models: []string{"llama2:7b"},
	})
	err := mm.enforceResolutionBounds(ctx, "chat", providerNamed("llama2:70b"), "b")
	require.ErrorIs(t, err, ErrResolutionOutOfBounds)

	ctx = WithResolutionBounds(context.Background(), ResolutionBounds{Models: []string{"gpt-4"}})
	require.ErrorIs(t, mm.enforceResolutionBounds(ctx, "chat", providerNamed("gpt4"), "b"), ErrResolutionOutOfBounds)
}

// An envelope with no compute block must resolve exactly as it did before bounds existed.
func TestUnit_ResolutionBounds_AbsentBoundsAreUnbounded(t *testing.T) {
	mm := boundedManager()
	require.NoError(t, mm.enforceResolutionBounds(context.Background(), "chat", providerNamed("anything-at-all"), "any-backend"))

	ctx := WithResolutionBounds(context.Background(), ResolutionBounds{})
	require.True(t, ResolutionBoundsFromContext(ctx).IsZero())
	require.NoError(t, mm.enforceResolutionBounds(ctx, "chat", providerNamed("anything-at-all"), "any-backend"))
}

// Bounding models alone must leave backend choice free, and vice versa.
func TestUnit_ResolutionBounds_DimensionsAreIndependent(t *testing.T) {
	mm := boundedManager()

	modelsOnly := WithResolutionBounds(context.Background(), ResolutionBounds{Models: []string{"m1"}})
	require.NoError(t, mm.enforceResolutionBounds(modelsOnly, "chat", providerNamed("m1"), "whatever-backend"))

	backendsOnly := WithResolutionBounds(context.Background(), ResolutionBounds{Backends: []string{"b1"}})
	require.NoError(t, mm.enforceResolutionBounds(backendsOnly, "chat", providerNamed("whatever-model"), "b1"))
	err := mm.enforceResolutionBounds(backendsOnly, "chat", providerNamed("whatever-model"), "b2")
	require.ErrorIs(t, err, ErrResolutionOutOfBounds)
	require.Contains(t, err.Error(), "backendAllowlist")
}

// A backend the runtime cannot name matches by id only — fails closed rather than admitting an unidentifiable backend.
func TestUnit_ResolutionBounds_UnnamedBackendMatchesByIDOnly(t *testing.T) {
	mm := boundedManager()

	byID := WithResolutionBounds(context.Background(), ResolutionBounds{Backends: []string{"openai-backend"}})
	require.NoError(t, mm.enforceResolutionBounds(byID, "chat", providerNamed("m"), "openai-backend"))

	byName := WithResolutionBounds(context.Background(), ResolutionBounds{Backends: []string{"my-ollama"}})
	require.ErrorIs(t, mm.enforceResolutionBounds(byName, "chat", providerNamed("m"), "openai-backend"), ErrResolutionOutOfBounds)
}

// A name in the allowlist must admit the backend the resolver returns by id; runs against real runtime state so the id->name lookup is proven, not mocked.
func TestUnit_ResolutionBounds_BackendMatchesOperatorFacingName(t *testing.T) {
	ctx, state, db := newReconcileTestState(t, runtimestate.WithAutoDiscoverModels())
	mm := &modelManager{runtime: state, tracker: libtracker.NoopTracker{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	defer server.Close()

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "backend-uuid-1",
		Name:    "my-openai",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	keyData, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, keyData))
	require.NoError(t, state.RunBackendCycle(ctx))

	named := WithResolutionBounds(ctx, ResolutionBounds{Backends: []string{"my-openai"}})
	require.NoError(t, mm.enforceResolutionBounds(named, "chat", providerNamed("gpt-5"), "backend-uuid-1"))

	other := WithResolutionBounds(ctx, ResolutionBounds{Backends: []string{"some-other-backend"}})
	rErr := mm.enforceResolutionBounds(other, "chat", providerNamed("gpt-5"), "backend-uuid-1")
	require.ErrorIs(t, rErr, ErrResolutionOutOfBounds)
	require.Contains(t, rErr.Error(), "my-openai")
	require.Contains(t, rErr.Error(), "backend-uuid-1")
}

// The refusal text must be stable across runs since it lands in a durable mission record.
func TestUnit_ResolutionBounds_RefusalTextIsDeterministic(t *testing.T) {
	mm := boundedManager()
	ctx := WithResolutionBounds(context.Background(), ResolutionBounds{
		Models: []string{"zeta", "alpha", "mid"},
	})
	first := mm.enforceResolutionBounds(ctx, "chat", providerNamed("nope"), "b").Error()
	for range 5 {
		require.Equal(t, first, mm.enforceResolutionBounds(ctx, "chat", providerNamed("nope"), "b").Error())
	}
	require.Contains(t, first, `"alpha", "mid", "zeta"`)
}

// A bounded context must stop Chat/Stream/PromptExecute/Embed before anything is sent, even though the backend would happily serve the request.
func TestUnit_ResolutionBounds_EnforcedOnEveryCallPath(t *testing.T) {
	ctx, state, db := newReconcileTestState(t, runtimestate.WithAutoDiscoverModels())
	mm := &modelManager{runtime: state, tracker: libtracker.NoopTracker{}}

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			called = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-5"}},
		})
	}))
	defer server.Close()

	store := runtimetypes.New(db.WithoutTransaction())
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		ID:      "backend-uuid-1",
		Name:    "my-openai",
		Type:    "openai",
		BaseURL: server.URL,
	}))
	keyData, err := json.Marshal(runtimestate.ProviderConfig{APIKey: "test-key", Type: "openai"})
	require.NoError(t, err)
	require.NoError(t, store.SetKV(ctx, runtimestate.OpenaiKey, keyData))
	require.NoError(t, state.RunBackendCycle(ctx))

	bounded := WithResolutionBounds(ctx, ResolutionBounds{Models: []string{"only-this-model"}})
	req := Request{ProviderTypes: []string{"openai"}, ModelNames: []string{"gpt-5"}}
	msgs := []libmodelprovider.Message{{Role: "user", Content: "hi"}}

	_, _, chatErr := mm.Chat(bounded, req, msgs)
	require.ErrorIs(t, chatErr, ErrResolutionOutOfBounds)

	_, _, streamErr := mm.Stream(bounded, req, msgs)
	require.ErrorIs(t, streamErr, ErrResolutionOutOfBounds)

	_, _, promptErr := mm.PromptExecute(bounded, req, "sys", 0, "hi")
	require.ErrorIs(t, promptErr, ErrResolutionOutOfBounds)

	require.False(t, called, "a refused resolution must not reach the provider")
}
