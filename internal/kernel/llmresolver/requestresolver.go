package llmresolver

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"

	"github.com/contenox/beam/internal/libtracker"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
)

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

	seenProviders := make(map[string]bool)
	var candidates []libmodelprovider.Provider

	if len(req.ModelNames) > 0 {
		// Preferred models are matched in priority order.
		for _, preferredModel := range req.ModelNames {
			normalizedPreferred := NormalizeModelName(preferredModel)

			for _, p := range providers {
				if seenProviders[p.GetID()] {
					continue
				}

				currentNormalized := NormalizeModelName(p.ModelName())
				currentFull := p.ModelName()
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
		for _, p := range providers {
			if validateProvider(p, req.ContextLength, capCheck) {
				candidates = append(candidates, p)
			}
		}
	}

	if len(candidates) == 0 {
		// Distinguish a context-only shortfall (capable, name-matched models
		// exist but all advertise less context than needed) from a plain
		// no-match, so the caller gets an actionable message.
		if req.ContextLength > 0 {
			largest := 0
			var largestName string
			capable := false
			for _, p := range providers {
				if !capCheck(p) {
					continue
				}
				if len(req.ModelNames) > 0 && !providerMatchesAnyName(p, req.ModelNames) {
					continue
				}
				capable = true
				if cl := p.GetContextLength(); cl > largest {
					largest = cl
					largestName = p.ModelName()
				}
			}
			if capable && largest > 0 && largest < req.ContextLength {
				return nil, fmt.Errorf("%w: request needs %d tokens of context but the largest available model %q provides only %d; use a larger-context model or reduce the request size (fewer tools or shorter history)",
					ErrNoSatisfactoryModel, req.ContextLength, largestName, largest)
			}
		}

		var builder strings.Builder

		builder.WriteString("no models matched requirements:\n")
		builder.WriteString(fmt.Sprintf("- provider: %q\n", providerTypes))
		builder.WriteString(fmt.Sprintf("- model names: %v\n", req.ModelNames))
		builder.WriteString(fmt.Sprintf("- required context length: %d\n", req.ContextLength))
		if req.RequiresVision {
			builder.WriteString("- requires vision (request carries images)\n")
		}

		builder.WriteString("- available models:\n")
		for _, p := range providers {
			builder.WriteString(fmt.Sprintf("  • %s (ID: %s, context: %d, canchat: %v, can embed: %v, canprompt: %v, canvision: %v)\n",
				p.ModelName(), p.GetID(), p.GetContextLength(), p.CanChat(), p.CanEmbed(), p.CanPrompt(), p.CanVision()))
		}

		return nil, fmt.Errorf("%w\n%s", ErrNoSatisfactoryModel, builder.String())
	}

	return candidates, nil
}

// providerMatchesAnyName reports whether p's model name matches any of names,
// comparing both the full name and the normalized form (the same match rule the
// candidate filter uses, minus its per-name priority ordering).
func providerMatchesAnyName(p libmodelprovider.Provider, names []string) bool {
	full := p.ModelName()
	normalized := NormalizeModelName(full)
	for _, name := range names {
		if full == name || normalized == NormalizeModelName(name) {
			return true
		}
	}
	return false
}

// validateProvider reports whether a provider meets requirements. A provider
// whose context length is 0 (unknown) is never rejected on context grounds —
// only models known to be insufficient are filtered out.
func validateProvider(p libmodelprovider.Provider, minContext int, capCheck func(libmodelprovider.Provider) bool) bool {
	cl := p.GetContextLength()
	if minContext > 0 && cl > 0 && cl < minContext {
		return false
	}
	return capCheck(p)
}

// NormalizeModelName standardizes model names for comparison: lowercased,
// separators and organization prefix stripped, quantization suffixes and any
// trailing ":version" removed.
func NormalizeModelName(modelName string) string {
	normalized := strings.ToLower(modelName)

	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	normalized = strings.ReplaceAll(normalized, ".", "")

	if parts := strings.Split(normalized, "/"); len(parts) > 1 {
		normalized = parts[1]
	}

	normalized = strings.ReplaceAll(normalized, "awq", "")
	normalized = strings.ReplaceAll(normalized, "gptq", "")
	normalized = strings.ReplaceAll(normalized, "4bit", "")
	normalized = strings.ReplaceAll(normalized, "fp16", "")

	if idx := strings.LastIndex(normalized, ":"); idx != -1 {
		normalized = normalized[:idx]
	}

	return normalized
}

// refusePinnedNonVision guards explicitly-pinned models on image-bearing
// requests: when the request names models and the highest-priority name that
// actually resolves to a capable provider lacks CanVision, the request is
// refused with a teaching error instead of silently falling through to a
// different (vision-capable) model later in the list — an explicit pin must
// never be swapped without saying so. Requests without model names keep
// capability-based auto-selection; pinned names that match no provider fall
// through to the ordinary no-match handling.
func refusePinnedNonVision(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	base func(libmodelprovider.Provider) bool,
) error {
	if !req.RequiresVision || len(req.ModelNames) == 0 {
		return nil
	}
	providers, err := getModels(ctx, req.ProviderTypes...)
	if err != nil {
		return nil // let filterCandidates surface the lookup failure
	}
	for _, name := range req.ModelNames {
		matched := false
		for _, p := range providers {
			if !providerMatchesAnyName(p, []string{name}) || !base(p) {
				continue
			}
			matched = true
			if p.CanVision() {
				return nil
			}
		}
		if matched {
			return fmt.Errorf("%w: requested model %q accepts text only and the request carries images; name a vision-capable model, set a vision-capable default, or drop the model pin to let routing pick one",
				ErrPinnedModelLacksVision, name)
		}
	}
	return nil
}

// visionCapCheck wraps a base capability check with the vision requirement
// when the request carries image attachments, so image-bearing requests only
// resolve to providers that report CanVision.
func visionCapCheck(req Request, base func(libmodelprovider.Provider) bool) func(libmodelprovider.Provider) bool {
	if !req.RequiresVision {
		return base
	}
	return func(p libmodelprovider.Provider) bool {
		return base(p) && p.CanVision()
	}
}

// classifyVisionFailure distinguishes a vision-capability shortfall from a
// plain no-match: when a vision-requiring request found no candidates but the
// same request without the vision requirement would have, the failure is the
// missing vision capability and the returned error says so. The request still
// fails either way — degrading to a text-only model would silently drop the
// images, which is never acceptable.
func classifyVisionFailure(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	base func(libmodelprovider.Provider) bool,
	err error,
) error {
	if !req.RequiresVision || !errors.Is(err, ErrNoSatisfactoryModel) {
		return err
	}
	textCandidates, textErr := filterCandidates(ctx, req, getModels, base)
	if textErr != nil || len(textCandidates) == 0 {
		return err
	}
	names := make([]string, 0, len(textCandidates))
	for _, p := range textCandidates {
		names = append(names, p.ModelName())
	}
	return fmt.Errorf("%w: matching models %v accept text only; use a vision-capable model for requests with images", ErrNoVisionCapableModel, names)
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
		"requires_vision", req.RequiresVision,
	)
	defer endFn()

	if err := refusePinnedNonVision(ctx, req, getModels, libmodelprovider.Provider.CanChat); err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	candidates, err := filterCandidates(ctx, req, getModels, visionCapCheck(req, libmodelprovider.Provider.CanChat))
	if err != nil {
		err = classifyVisionFailure(ctx, req, getModels, libmodelprovider.Provider.CanChat, err)
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
	client, err := provider.GetChatConnection(libmodelprovider.WithRequestedContextLength(ctx, req.ContextLength), backend)
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
		"requires_vision", req.RequiresVision,
	)
	defer endFn()

	if err := refusePinnedNonVision(ctx, req, getModels, libmodelprovider.Provider.CanStream); err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	candidates, err := filterCandidates(ctx, req, getModels, visionCapCheck(req, libmodelprovider.Provider.CanStream))
	if err != nil {
		err = classifyVisionFailure(ctx, req, getModels, libmodelprovider.Provider.CanStream, err)
		reportErr(err)
		return nil, nil, "", err
	}
	provider, backend, err := resolver(candidates)
	if err != nil {
		reportErr(err)
		return nil, nil, "", err
	}
	client, err := provider.GetStreamConnection(libmodelprovider.WithRequestedContextLength(ctx, req.ContextLength), backend)
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
	client, err := provider.GetPromptConnection(libmodelprovider.WithRequestedContextLength(ctx, req.ContextLength), backend)
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

// Policy selects one provider and one of its backends from the candidate set.
// Randomly is the canonical Policy value; StickyOrRandom builds session-affine
// ones.
type Policy = func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error)

// StickyOrRandom returns a Policy that pins a session to one provider+backend
// via rendezvous (highest-random-weight) hashing over the candidate set: the
// same sessionKey picks the same provider+backend for as long as that pair
// stays in the candidate set, and when it leaves only the sessions pinned to
// it move. Distinct keys spread across the set. An empty key behaves exactly
// like Randomly.
func StickyOrRandom(sessionKey string) Policy {
	if sessionKey == "" {
		return Randomly
	}
	return func(candidates []libmodelprovider.Provider) (libmodelprovider.Provider, string, error) {
		if len(candidates) == 0 {
			return nil, "", ErrNoSatisfactoryModel
		}
		var (
			best        libmodelprovider.Provider
			bestBackend string
			bestScore   uint64
			found       bool
		)
		for _, p := range candidates {
			if p == nil {
				continue
			}
			for _, backend := range p.GetBackendIDs() {
				score := rendezvousScore(sessionKey, p.GetID(), backend)
				if !found || score > bestScore {
					best, bestBackend, bestScore, found = p, backend, score, true
				}
			}
		}
		if !found {
			return nil, "", ErrNoSatisfactoryModel
		}
		return best, bestBackend, nil
	}
}

// rendezvousScore is the weight of one (session, provider, backend) triple.
// FNV-1a over the joined identity; the delimiter keeps distinct triples from
// colliding by concatenation.
func rendezvousScore(sessionKey, providerID, backendID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(providerID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(backendID))
	return h.Sum64()
}

// Randomly is a Policy that selects a random provider and random backend.
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

// ErrNoVisionCapableModel is returned when a request carries image attachments
// but no candidate model supports vision. It wraps ErrNoSatisfactoryModel so
// existing no-match handling (e.g. the resolution self-heal cycle) still fires.
var ErrNoVisionCapableModel = fmt.Errorf("%w: no available model supports image input (vision)", ErrNoSatisfactoryModel)

// ErrPinnedModelLacksVision is returned when an image-bearing request
// explicitly names a model that exists but cannot accept images. It does NOT
// wrap ErrNoSatisfactoryModel: the refusal is deterministic, so retry /
// self-heal cycles must not fire for it.
var ErrPinnedModelLacksVision = errors.New("explicitly requested model lacks vision capability")

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
