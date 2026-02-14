package llmresolver

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"

	libmodelprovider "github.com/contenox/vibe/internal/modelrepo"
	"github.com/contenox/vibe/libtracker"
)

type RequestResolver struct {
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error)
	resolver  func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error)
}

// ResolveChat implements Resolver by using the struct's getModels and resolver fields.
func (r *RequestResolver) ResolveChat(ctx context.Context, req Request) (libmodelprovider.LLMChatClient, libmodelprovider.Provider, string, error) {
	return Chat(ctx, req, r.getModels, r.resolver)
}

// ResolvePromptExecute implements Resolver by using the struct's getModels and resolver fields.
func (r *RequestResolver) ResolvePromptExecute(ctx context.Context, req Request) (libmodelprovider.LLMPromptExecClient, libmodelprovider.Provider, string, error) {
	return PromptExecute(ctx, req, r.getModels, r.resolver)
}

// ResolveEmbed implements Resolver by using the struct's getModels and resolver fields.
func (r *RequestResolver) ResolveEmbed(ctx context.Context, req EmbedRequest) (libmodelprovider.LLMEmbedClient, libmodelprovider.Provider, string, error) {
	return Embed(ctx, req, r.getModels, r.resolver)
}

// ResolveStream implements Resolver by using the struct's getModels and resolver fields.
func (r *RequestResolver) ResolveStream(ctx context.Context, req Request) (libmodelprovider.LLMStreamClient, libmodelprovider.Provider, string, error) {
	return Stream(ctx, req, r.getModels, r.resolver)
}

// NewRequestResolver creates a new Resolver implementation with the specified dependencies.
// This is the preferred way to instantiate a resolver.
func NewRequestResolver(
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	resolver func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error),
) Resolver {
	return &RequestResolver{
		getModels: getModels,
		resolver:  resolver,
	}
}

func filterCandidates(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	capCheck func(libmodelprovider.Provider) bool,
) ([]libmodelprovider.Provider, error) {
	providerTypes := req.ProviderTypes
	providers, err := getModels(ctx, providerTypes...)
	if err != nil {
		return nil, fmt.Errorf("failed to get models: %w", err)
	}
	if len(providers) == 0 {
		return nil, ErrNoAvailableModels
	}

	// Use a map to track seen providers by ID to prevent duplicates
	seenProviders := make(map[string]bool)
	var candidates []libmodelprovider.Provider

	// Handle model name preferences
	if len(req.ModelNames) > 0 {
		// Check preferred models in order of priority
		for _, preferredModel := range req.ModelNames {
			// Use normalized model names for matching
			normalizedPreferred := NormalizeModelName(preferredModel)

			for _, p := range providers {
				if seenProviders[p.GetID()] {
					continue
				}

				// Normalize provider's model name for comparison
				currentNormalized := NormalizeModelName(p.ModelName())
				currentFull := p.ModelName()

				// Match either normalized or full name
				if currentNormalized != normalizedPreferred && currentFull != preferredModel {
					continue
				}

				if validateProvider(p, req.ContextLength, capCheck) {
					candidates = append(candidates, p)
					seenProviders[p.GetID()] = true
				}
			}
		}
	} else {
		// Consider all providers when no model names specified
		for _, p := range providers {
			if validateProvider(p, req.ContextLength, capCheck) {
				candidates = append(candidates, p)
			}
		}
	}

	if len(candidates) == 0 {
		var builder strings.Builder

		builder.WriteString("no models matched requirements:\n")
		builder.WriteString(fmt.Sprintf("- provider: %q\n", providerTypes))
		builder.WriteString(fmt.Sprintf("- model names: %v\n", req.ModelNames))
		builder.WriteString(fmt.Sprintf("- required context length: %d\n", req.ContextLength))

		builder.WriteString("- available models:\n")
		for _, p := range providers {
			builder.WriteString(fmt.Sprintf("  • %s (ID: %s, context: %d, canchat: %v, can embed: %v, canprompt: %v)\n",
				p.ModelName(), p.GetID(), p.GetContextLength(), p.CanChat(), p.CanEmbed(), p.CanPrompt()))
		}

		return nil, fmt.Errorf("%w\n%s", ErrNoSatisfactoryModel, builder.String())
	}

	return candidates, nil
}

// validateProvider checks if a provider meets requirements
func validateProvider(p libmodelprovider.Provider, minContext int, capCheck func(libmodelprovider.Provider) bool) bool {
	if minContext > 0 && p.GetContextLength() < minContext {
		return false
	}
	return capCheck(p)
}

// NormalizeModelName standardizes model names for comparison
func NormalizeModelName(modelName string) string {
	// Convert to lowercase for case-insensitive comparison
	normalized := strings.ToLower(modelName)

	// Remove common prefixes and suffixes
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	normalized = strings.ReplaceAll(normalized, ".", "")

	// Remove organization prefix if present
	if parts := strings.Split(normalized, "/"); len(parts) > 1 {
		normalized = parts[1]
	}

	// Remove quantization suffixes
	normalized = strings.ReplaceAll(normalized, "awq", "")
	normalized = strings.ReplaceAll(normalized, "gptq", "")
	normalized = strings.ReplaceAll(normalized, "4bit", "")
	normalized = strings.ReplaceAll(normalized, "fp16", "")

	// Remove version numbers
	if idx := strings.LastIndex(normalized, ":"); idx != -1 {
		normalized = normalized[:idx]
	}

	return normalized
}

// Chat implements the chat resolution workflow using the provided dependencies.
func Chat(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	resolver func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error),
) (libmodelprovider.LLMChatClient, libmodelprovider.Provider, string, error) {
	tracker := req.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, reportChange, endFn := tracker.Start(
		ctx,
		"resolve",
		"chat_model",
		"provider_types", req.ProviderTypes,
		"model_names", req.ModelNames,
		"context_length", req.ContextLength,
	)
	defer endFn()

	candidates, err := filterCandidates(ctx, req, getModels, libmodelprovider.Provider.CanChat)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	provider, backend, err := resolver(candidates)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	if req.ContextLength < 0 {
		err := fmt.Errorf("context length must be non-negative")
		reportErr(err)
		return nil, nil, "", err
	}
	client, err := provider.GetChatConnection(ctx, backend)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	reportChange("selected_provider", map[string]string{
		"model_name":  provider.ModelName(),
		"provider_id": provider.GetID(),
		"backend_id":  backend,
	})
	return client, provider, backend, nil
}

// Embed implements the embedding resolution workflow using the provided dependencies.
func Embed(
	ctx context.Context,
	embedReq EmbedRequest,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	resolver func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error),
) (libmodelprovider.LLMEmbedClient, libmodelprovider.Provider, string, error) {
	tracker := embedReq.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, reportChange, endFn := tracker.Start(
		ctx,
		"resolve",
		"embed_model",
		"model_name", embedReq.ModelName,
		"provider_type", embedReq.ProviderType,
	)
	defer endFn()

	if embedReq.ModelName == "" {
		err := errors.New("model name is required")
		reportErr(err)
		return nil, nil, "", err
	}
	req := Request{
		ModelNames:    []string{embedReq.ModelName},
		ProviderTypes: []string{embedReq.ProviderType},
	}
	candidates, err := filterCandidates(ctx, req, getModels, libmodelprovider.Provider.CanEmbed)
	if err != nil {
		reportErr(err)
		return nil, nil, "", fmt.Errorf("failed to filter candidates: %w", err)
	}
	provider, backend, err := resolver(candidates)
	if err != nil {
		reportErr(err)
		return nil, nil, "", fmt.Errorf("failed to apply resolver: %w", err)
	}
	client, err := provider.GetEmbedConnection(ctx, backend)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	reportChange("selected_provider", map[string]string{
		"model_name":  provider.ModelName(),
		"provider_id": provider.GetID(),
		"backend_id":  backend,
	})
	return client, provider, backend, nil
}

// Stream implements the streaming resolution workflow using the provided dependencies.
func Stream(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	resolver func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error),
) (libmodelprovider.LLMStreamClient, libmodelprovider.Provider, string, error) {
	tracker := req.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, reportChange, endFn := tracker.Start(
		ctx,
		"resolve",
		"stream_model",
		"provider_types", req.ProviderTypes,
		"model_names", req.ModelNames,
		"context_length", req.ContextLength,
	)
	defer endFn()

	candidates, err := filterCandidates(ctx, req, getModels, libmodelprovider.Provider.CanStream)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	provider, backend, err := resolver(candidates)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	client, err := provider.GetStreamConnection(ctx, backend)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	reportChange("selected_provider", map[string]string{
		"model_name":  provider.ModelName(),
		"provider_id": provider.GetID(),
		"backend_id":  backend,
	})
	return client, provider, backend, nil
}

// PromptExecute implements the prompt execution resolution workflow using the provided dependencies.
func PromptExecute(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	resolver func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error),
) (libmodelprovider.LLMPromptExecClient, libmodelprovider.Provider, string, error) {
	tracker := req.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, reportChange, endFn := tracker.Start(
		ctx,
		"resolve",
		"prompt_model",
		"model_names", req.ModelNames,
		"provider_types", req.ProviderTypes,
		"context_length", req.ContextLength,
	)
	defer endFn()

	if len(req.ModelNames) == 0 {
		err := errors.New("at least one model name is required")
		reportErr(err)
		return nil, nil, "", err
	}
	candidates, err := filterCandidates(ctx, req, getModels, libmodelprovider.Provider.CanPrompt)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	provider, backend, err := resolver(candidates)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	client, err := provider.GetPromptConnection(ctx, backend)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	reportChange("selected_provider", map[string]string{
		"model_name":    provider.ModelName(),
		"provider_id":   provider.GetID(),
		"provider_type": provider.GetType(),
		"backend_id":    backend,
	})
	return client, provider, backend, nil
}

// Randomly is a policy that selects a random provider and random backend.
//
// This provides basic load balancing across available resources.
func Randomly(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error) {
	provider, err := selectRandomProvider(candidates)
	if err != nil {
		return nil, "", err
	}

	backend, err := selectRandomBackend(provider)
	if err != nil {
		return nil, "", err
	}

	return provider, backend, nil
}

// ErrNoAvailableModels is returned when no providers are available.
var ErrNoAvailableModels = errors.New("no models found in runtime state")

// ErrNoSatisfactoryModel is returned when providers exist but none match requirements.
var ErrNoSatisfactoryModel = errors.New("no model matched the requirements")

// HighestContext is a policy that selects the provider with the largest context window.
//
// When multiple providers have the same context length, one is chosen randomly.
// This is useful for tasks requiring long context windows.
func HighestContext(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error) {
	if len(candidates) == 0 {
		return nil, "", ErrNoSatisfactoryModel
	}

	var bestProvider libmodelprovider.Provider = nil
	maxContextLength := -1

	for _, p := range candidates {
		currentContextLength := p.GetContextLength()
		if currentContextLength > maxContextLength {
			maxContextLength = currentContextLength
			bestProvider = p
		}
	}

	if bestProvider == nil {
		return nil, "", errors.New("failed to select a provider based on context length") // Should never happen
	}

	// Once the best provider is selected, choose a backend randomly for it
	backend, err := selectRandomBackend(bestProvider)
	if err != nil {
		return nil, "", err
	}

	return bestProvider, backend, nil
}

func selectRandomBackend(provider libmodelprovider.Provider) (string, error) {
	if provider == nil {
		return "", ErrNoSatisfactoryModel
	}

	backendIDs := provider.GetBackendIDs()
	if len(backendIDs) == 0 {
		return "", ErrNoSatisfactoryModel
	}

	return backendIDs[rand.Intn(len(backendIDs))], nil
}

func selectRandomProvider(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, error) {
	if len(candidates) == 0 {
		return nil, ErrNoSatisfactoryModel
	}

	return candidates[rand.Intn(len(candidates))], nil
}

const (
	StrategyRandom      = "random"
	StrategyAuto        = "auto"
	StrategyLowLatency  = "low-latency"
	StrategyLowPriority = "low-prio"
)

// PolicyFromString maps string names to resolver policies
func PolicyFromString(name string) (func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error), error) {
	switch strings.ToLower(name) {
	case StrategyRandom:
		return Randomly, nil
	case StrategyLowLatency, StrategyAuto:
		return HighestContext, nil
	// case StrategyLowPriority:
	// 	return ResolveLowestPriority, nil
	default:
		return nil, fmt.Errorf("unknown resolver strategy: %s", name)
	}
}
