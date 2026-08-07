package runtimestate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/models/modelcapability"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libkvstore"
)

// observedModelCachePrefix is the KV key prefix under which each backend's
// observed model list is cached (see loadObservedModelCache /
// storeObservedModelCache). Distinct from ProviderKeyPrefix ("cloud-provider:").
const observedModelCachePrefix = "prov:"

func observedModelCacheKey(backendID string) string {
	return observedModelCachePrefix + backendID
}

// InvalidateModelCache removes the cached observed-model list for one backend,
// forcing the next backend cycle to refetch from the provider. Safe when no
// entry exists; no-op when kv is nil.
func InvalidateModelCache(ctx context.Context, kv libkvstore.KVManager, backendID string) error {
	if kv == nil {
		return nil
	}
	exec, err := kv.Executor(ctx)
	if err != nil {
		return err
	}
	return exec.Delete(ctx, observedModelCacheKey(backendID))
}

// ClearModelCache removes every cached observed-model list (all "prov:*" keys)
// and returns how many were cleared. No-op (0) when kv is nil.
func ClearModelCache(ctx context.Context, kv libkvstore.KVManager) (int, error) {
	if kv == nil {
		return 0, nil
	}
	exec, err := kv.Executor(ctx)
	if err != nil {
		return 0, err
	}
	keys, err := exec.Keys(ctx, observedModelCachePrefix+"*")
	if err != nil {
		return 0, err
	}
	cleared := 0
	for _, k := range keys {
		if err := exec.Delete(ctx, k); err != nil {
			return cleared, err
		}
		cleared++
	}
	return cleared, nil
}

func providerConfigKey(backendType string) (string, bool) {
	switch modelrepo.CanonicalBackendType(backendType) {
	case "ollama":
		return OllamaKey, true
	case "openai":
		return OpenaiKey, true
	case "anthropic":
		return AnthropicKey, true
	case "bedrock":
		return BedrockKey, true
	case "gemini":
		return GeminiKey, true
	case "vllm":
		// vLLM reuses the OpenAI-compatible bearer token configuration.
		return OpenaiKey, true
	case "vertex-google":
		return VertexGoogleKey, true
	default:
		return "", false
	}
}

func (s *State) loadProviderAPIKey(ctx context.Context, backendType string) (string, error) {
	key, ok := providerConfigKey(backendType)
	if !ok {
		return "", nil
	}

	cfg := ProviderConfig{}
	store := runtimetypes.New(s.dbInstance.WithoutTransaction())
	if err := store.GetKV(ctx, key, &cfg); err != nil {
		return "", err
	}
	if cfg.APIKey == "" && strings.TrimSpace(cfg.APIKeyEnv) != "" {
		return os.Getenv(strings.TrimSpace(cfg.APIKeyEnv)), nil
	}
	return cfg.APIKey, nil
}

func (s *State) newCatalogProvider(backend *runtimetypes.Backend, apiKey string) (modelrepo.CatalogProvider, error) {
	return modelrepo.NewCatalogProvider(
		modelrepo.BackendSpec{
			Type:    backend.Type,
			BaseURL: backend.BaseURL,
			APIKey:  apiKey,
		},
		modelrepo.WithCatalogHTTPClient(modelrepo.SharedHTTPClient),
	)
}

func (s *State) loadObservedModelCache(ctx context.Context, backendID, apiKey string) ([]modelrepo.ObservedModel, bool) {
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if raw, err := exec.Get(ctx, observedModelCacheKey(backendID)); err == nil {
				var entry providerCacheEntry
				if json.Unmarshal(raw, &entry) == nil && entry.APIKey == apiKey && len(entry.Models) > 0 {
					return entry.Models, true
				}
			}
		}
		return nil, false
	}

	if cached, ok := s.providerCache.Load(backendID); ok {
		if entry, ok := cached.(providerCacheEntry); ok && entry.APIKey == apiKey && len(entry.Models) > 0 {
			return entry.Models, true
		}
	}
	return nil, false
}

func (s *State) storeObservedModelCache(ctx context.Context, backendID, apiKey string, models []modelrepo.ObservedModel) {
	entry := providerCacheEntry{Models: models, APIKey: apiKey}
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if data, err := json.Marshal(entry); err == nil {
				_ = exec.SetWithTTL(ctx, observedModelCacheKey(backendID), data, ProviderCacheDuration)
			}
		}
		return
	}
	s.providerCache.Store(backendID, entry)
}

func observedModelNames(models []modelrepo.ObservedModel) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	return names
}

func (s *State) applyCapabilityOverrides(ctx context.Context, provider string, model ModelPullStatus) ModelPullStatus {
	provider = modelrepo.CanonicalBackendType(provider)
	name := strings.TrimSpace(model.Model)
	if name == "" {
		name = strings.TrimSpace(model.Name)
	}
	if name == "" {
		return model
	}
	override, ok, err := modelcapability.New(runtimetypes.New(s.dbInstance.WithoutTransaction())).Get(ctx, provider, name)
	if err != nil || !ok {
		return model
	}
	if override.CanThink != nil {
		model.CanThink = *override.CanThink
	}
	if override.CanVision != nil {
		model.CanVision = *override.CanVision
	}
	return model
}

func storeBackendError(state *State, backend *runtimetypes.Backend, apiKey string, err error, models []string) {
	runtimeState := &BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Models:       models,
		PulledModels: []ModelPullStatus{},
		Backend:      *backend,
	}
	if err != nil {
		runtimeState.Error = err.Error()
	}
	runtimeState.SetAPIKey(apiKey)
	state.state.Store(backend.ID, runtimeState)
}

func declaredModelDebugMap(declaredModels map[string]*runtimetypes.Model) []string {
	declaredMap := make([]string, 0, len(declaredModels))
	for key, model := range declaredModels {
		payload := "model-data==nil"
		if model != nil {
			payload = model.ID + " " + model.Model
		}
		declaredMap = append(declaredMap, key+":"+payload)
	}
	return declaredMap
}

func declaredModelsUnavailableError(provider string, declaredModels map[string]*runtimetypes.Model, available []string) error {
	return fmt.Errorf(
		"None of the declared models are available in the %s API: declared models: %v \navailable models %s",
		provider,
		strings.Join(declaredModelDebugMap(declaredModels), ", "),
		available,
	)
}
