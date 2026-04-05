package runtimestate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/contenox/contenox/internal/modelrepo"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/statetype"
)

func providerConfigKey(backendType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(backendType)) {
	case "ollama":
		return OllamaKey, true
	case "openai":
		return OpenaiKey, true
	case "gemini":
		return GeminiKey, true
	case "vllm":
		// vLLM reuses the OpenAI-compatible bearer token configuration.
		return OpenaiKey, true
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
	return cfg.APIKey, nil
}

func (s *State) newCatalogProvider(backend *runtimetypes.Backend, apiKey string) (modelrepo.CatalogProvider, error) {
	return modelrepo.NewCatalogProvider(
		modelrepo.BackendSpec{
			Type:    backend.Type,
			BaseURL: backend.BaseURL,
			APIKey:  apiKey,
		},
		modelrepo.WithCatalogHTTPClient(http.DefaultClient),
	)
}

func (s *State) loadObservedModelCache(ctx context.Context, backendID, apiKey string) ([]modelrepo.ObservedModel, bool) {
	if s.kvStore != nil {
		if exec, err := s.kvStore.Executor(ctx); err == nil {
			if raw, err := exec.Get(ctx, "prov:"+backendID); err == nil {
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
				_ = exec.SetWithTTL(ctx, "prov:"+backendID, data, ProviderCacheDuration)
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

func storeBackendError(state *State, backend *runtimetypes.Backend, apiKey string, err error, models []string) {
	runtimeState := &statetype.BackendRuntimeState{
		ID:           backend.ID,
		Name:         backend.Name,
		Models:       models,
		PulledModels: []statetype.ModelPullStatus{},
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
