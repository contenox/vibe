package runtimestate

import (
	"context"

	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
)

// LocalProviderAdapter creates providers for self-hosted backends (Ollama, vLLM).
func LocalProviderAdapter(ctx context.Context, tracker libtracker.ActivityTracker, runtime map[string]BackendRuntimeState) ProviderFromRuntimeState {
	// One flat list of providers, one per model per backend.
	providersByType := make(map[string][]modelrepo.Provider)

	for _, state := range runtime {
		if state.Error != "" {
			continue
		}

		backendType := modelrepo.CanonicalBackendType(state.Backend.Type)
		catalog, err := modelrepo.NewCatalogProvider(
			modelrepo.BackendSpec{
				Type:    backendType,
				BaseURL: state.Backend.BaseURL,
				APIKey:  state.GetAPIKey(),
			},
			modelrepo.WithCatalogHTTPClient(modelrepo.SharedHTTPClient),
			modelrepo.WithCatalogTracker(tracker),
		)
		if err != nil {
			continue
		}
		if _, ok := providersByType[backendType]; !ok {
			providersByType[backendType] = []modelrepo.Provider{}
		}

		for _, model := range state.PulledModels {
			providersByType[backendType] = append(
				providersByType[backendType],
				catalog.ProviderFor(observedModelFromPullStatus(model)),
			)
		}
	}

	return func(ctx context.Context, backendTypes ...string) ([]modelrepo.Provider, error) {
		// No specific backend types requested (or only empty strings from an
		// unconfigured default provider): return providers from every backend type.
		hasNonEmpty := false
		for _, bt := range backendTypes {
			if bt != "" {
				hasNonEmpty = true
				break
			}
		}
		if !hasNonEmpty {
			var all []modelrepo.Provider
			for _, providers := range providersByType {
				all = append(all, providers...)
			}
			return all, nil
		}
		var providers []modelrepo.Provider
		for _, backendType := range backendTypes {
			backendType = modelrepo.CanonicalBackendType(backendType)
			if typeProviders, ok := providersByType[backendType]; ok {
				providers = append(providers, typeProviders...)
			}
		}
		return providers, nil
	}
}

// ProviderFromRuntimeState retrieves available model providers
type ProviderFromRuntimeState func(ctx context.Context, backendTypes ...string) ([]modelrepo.Provider, error)
