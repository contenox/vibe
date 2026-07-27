package llmrepo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/contenox/beam/internal/kernel/llmresolver"
	"github.com/contenox/beam/internal/libtracker"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/models/runtimestate"
)

var _ ModelRepo = (*modelManager)(nil)

// Unified Request type for all operations
type Request struct {
	ProviderTypes []string // Optional: if empty, uses all default providers
	ModelNames    []string // Optional: if empty, any model is considered
	ContextLength int      // Minimum required context length
	// SessionKey is an opaque per-session cache-affinity key (derive it with
	// DeriveSessionKey — never pass a raw internal session ID). A non-empty
	// key pins the whole session to one provider/backend so server-side
	// prefix caches stay warm; empty keeps today's random resolution. When
	// the construction site has no session identity the key may also travel
	// on the context via WithSessionKey; an explicit field value wins.
	SessionKey string
	// CacheHints declares where the stable/volatile boundary of this request
	// lies (see CacheHints). Optional; nil changes nothing.
	CacheHints *CacheHints
	Tracker    libtracker.ActivityTracker
}

type EmbedRequest struct {
	ModelName    string
	ProviderType string
	Tracker      libtracker.ActivityTracker
}

type Meta struct {
	ModelName    string `json:"model_name"`
	ProviderType string `json:"provider_type"`
	BackendID    string `json:"backend_id"`
	// Usage is the provider-reported token accounting for the call, when the
	// operation carries no other channel for it (PromptExecute); nil means the
	// provider did not report usage. Chat/Stream report usage on the
	// ChatResult / terminal StreamParcel instead.
	Usage *libmodelprovider.TokenUsage `json:"usage,omitempty"`
}

type ModelRepo interface {
	Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error)
	CountTokens(ctx context.Context, modelName string, prompt string) (int, error)
	PromptExecute(
		ctx context.Context,
		req Request,
		systeminstruction string, temperature float32, prompt string,
	) (string, Meta, error)
	Chat(
		ctx context.Context,
		req Request,
		Messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument,
	) (libmodelprovider.ChatResult, Meta, error)
	Embed(
		ctx context.Context,
		embedReq EmbedRequest,
		prompt string,
	) ([]float64, Meta, error)
	Stream(
		ctx context.Context,
		req Request,
		messages []libmodelprovider.Message,
		opts ...libmodelprovider.ChatArgument,
	) (<-chan *libmodelprovider.StreamParcel, Meta, error)
}

type Tokenizer interface {
	Tokenize(ctx context.Context, prompt string) ([]int, error)
	CountTokens(ctx context.Context, prompt string) (int, error)
}

var _ ModelRepo = (*modelManager)(nil)

type modelManager struct {
	runtime   *runtimestate.State
	tokenizer ollamatokenizer.Tokenizer
	config    ModelManagerConfig
	mu        sync.RWMutex
	tracker   libtracker.ActivityTracker

	// reconcileMu serializes the resolution self-heal cycle and lastReconcileAt
	// debounces it; see reconcileForResolution.
	reconcileMu     sync.Mutex
	lastReconcileAt time.Time
}

// minResolveReconcileInterval debounces the resolution-failure backend cycle so a
// burst of failing requests coalesces into a single re-scan.
const minResolveReconcileInterval = 5 * time.Second

// reconcileForResolution self-heals a runtime that resolved its model state
// before a backend was reachable. The runtime reconciles backends at startup and
// on explicit refresh only (no periodic loop), so a backend that comes up
// afterwards — most commonly a local backend (ollama, vllm) being (re)started after the runtime — stays
// invisible and every request for its models fails with "no models found in
// runtime state". When resolution fails for that reason this runs one debounced
// backend cycle and reports whether the caller should retry resolution. It fires
// only for the resolver's no-models / no-match errors so genuine downstream
// failures are never retried.
func (e *modelManager) reconcileForResolution(ctx context.Context, resolveErr error) bool {
	if !errors.Is(resolveErr, llmresolver.ErrNoAvailableModels) && !errors.Is(resolveErr, llmresolver.ErrNoSatisfactoryModel) {
		return false
	}
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	// A very recent cycle already refreshed state (e.g. a concurrent failing
	// request); retry against it instead of re-scanning every backend again.
	if !e.lastReconcileAt.IsZero() && time.Since(e.lastReconcileAt) < minResolveReconcileInterval {
		return true
	}
	err := e.runtime.RunBackendCycle(ctx)
	e.lastReconcileAt = time.Now()
	return err == nil
}

type ModelConfig struct {
	Name     string
	Provider string
}

type ModelManagerConfig struct {
	DefaultPromptModel    ModelConfig
	DefaultEmbeddingModel ModelConfig
	DefaultChatModel      ModelConfig
}

func NewModelManager(runtime *runtimestate.State, tokenizer ollamatokenizer.Tokenizer, config ModelManagerConfig, tracker libtracker.ActivityTracker) (*modelManager, error) {
	if runtime == nil {
		return nil, errors.New("runtime cannot be nil")
	}
	if tokenizer == nil {
		return nil, errors.New("tokenizer cannot be nil")
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &modelManager{
		runtime:   runtime,
		tokenizer: tokenizer,
		config:    config,
		tracker:   tracker,
	}, nil
}

func (e *modelManager) Tokenize(ctx context.Context, modelName string, prompt string) ([]int, error) {
	if prompt == "" {
		return []int{}, nil
	}

	tokenizer, err := e.GetTokenizer(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("failed to get tokenizer: %w", err)
	}

	tokens, err := tokenizer.Tokenize(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("tokenization failed: %w", err)
	}

	return tokens, nil
}

func (e *modelManager) CountTokens(ctx context.Context, modelName string, prompt string) (int, error) {
	if prompt == "" {
		return 0, nil
	}

	tokenizer, err := e.GetTokenizer(ctx, modelName)
	if err != nil {
		return 0, fmt.Errorf("failed to get tokenizer: %w", err)
	}

	count, err := tokenizer.CountTokens(ctx, prompt)
	if err != nil {
		return 0, fmt.Errorf("token counting failed: %w", err)
	}

	return count, nil
}

func (e *modelManager) PromptExecute(
	ctx context.Context,
	req Request,
	systemInstruction string, temperature float32, prompt string,
) (string, Meta, error) {
	if err := validateRequest(req); err != nil {
		return "", Meta{}, fmt.Errorf("invalid request: %w", err)
	}

	runtimeStateResolution := e.GetRuntime(ctx)

	// Apply defaults if not provided
	if len(req.ModelNames) == 0 {
		req.ModelNames = []string{e.config.DefaultPromptModel.Name}
	}
	if len(req.ProviderTypes) == 0 {
		req.ProviderTypes = []string{e.config.DefaultPromptModel.Provider}
	}

	resolverReq := e.convertToResolverRequest(req, nil)
	// Session-sticky resolution (blueprint §4.1.5): with a session key the
	// same session lands on the same provider/backend so its prefix cache
	// stays warm; without one this is exactly the old Randomly policy.
	policy := llmresolver.StickyOrRandom(effectiveSessionKey(ctx, req))
	client, provider, backend, err := llmresolver.PromptExecute(ctx,
		resolverReq,
		runtimeStateResolution,
		policy,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.PromptExecute(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			policy,
		)
	}
	if err != nil {
		return "", Meta{}, fmt.Errorf("prompt execute: client resolution failed: %w", err)
	}
	defer safeClose(client)

	// The envelope's model/backend allowlist, checked between resolution and the
	// call so a refusal spends nothing (see bounds.go). Unbounded requests skip it.
	if err := e.enforceResolutionBounds(ctx, "prompt", provider, backend); err != nil {
		return "", Meta{}, err
	}

	result, usage, err := client.Prompt(ctx, systemInstruction, temperature, prompt)
	if err != nil {
		return "", Meta{}, fmt.Errorf("prompt execution failed: %w", err)
	}

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
		Usage:        usage,
	}
	// Same tracker seam as the chat path so token accounting is observable on
	// every non-streaming call.
	if usage != nil {
		e.reportTokenUsage(ctx, req, meta, *usage)
	}
	return result, meta, nil
}

func (e *modelManager) Chat(
	ctx context.Context,
	req Request,
	messages []libmodelprovider.Message, opts ...libmodelprovider.ChatArgument,
) (libmodelprovider.ChatResult, Meta, error) {
	if err := validateRequest(req); err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("invalid request: %w", err)
	}

	if len(messages) == 0 {
		return libmodelprovider.ChatResult{}, Meta{}, errors.New("messages cannot be empty")
	}

	runtimeStateResolution := e.GetRuntime(ctx)

	// Apply defaults if not provided
	if len(req.ModelNames) == 0 {
		req.ModelNames = []string{e.config.DefaultChatModel.Name}
	}
	if len(req.ProviderTypes) == 0 {
		req.ProviderTypes = []string{e.config.DefaultChatModel.Provider}
	}

	resolverReq := e.convertToResolverRequest(req, messages)
	// Session-sticky resolution (blueprint §4.1.5); empty key == Randomly.
	sessionKey := effectiveSessionKey(ctx, req)
	policy := llmresolver.StickyOrRandom(sessionKey)
	client, provider, backend, err := llmresolver.Chat(ctx,
		resolverReq,
		runtimeStateResolution,
		policy,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.Chat(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			policy,
		)
	}
	if err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat: client resolution failed: %w", err)
	}
	defer safeClose(client)

	// Envelope allowlist, checked before anything is sent (see bounds.go).
	if err := e.enforceResolutionBounds(ctx, "chat", provider, backend); err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, err
	}

	response, err := client.Chat(ctx, messages, withCanonicalRequestShape(opts, providerCacheHints(req.CacheHints, sessionKey))...)
	if err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat execution failed: %w", err)
	}

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
	}
	// Non-streaming usage reporting: providers attach their token accounting
	// to the ChatResult; record it on the same tracker event the stream path
	// uses so cache hit rates are observable on both paths.
	if response.Usage != nil {
		e.reportTokenUsage(ctx, req, meta, *response.Usage)
	}
	return response, meta, nil
}

func (e *modelManager) Embed(
	ctx context.Context,
	embedReq EmbedRequest,
	prompt string,
) ([]float64, Meta, error) {
	if prompt == "" {
		return nil, Meta{}, errors.New("prompt cannot be empty")
	}

	runtimeStateResolution := e.GetRuntime(ctx)

	// Apply defaults if not provided
	if embedReq.ModelName == "" {
		embedReq.ModelName = e.config.DefaultEmbeddingModel.Name
	}
	if embedReq.ProviderType == "" {
		embedReq.ProviderType = e.config.DefaultEmbeddingModel.Provider
	}

	resolverReq := e.convertToResolverEmbedRequest(embedReq)
	// Embedding stays on Randomly deliberately: EmbedRequest carries no
	// session identity, and embeddings gain nothing from prefix caches (each
	// request is an independent document, not an append-only conversation).
	client, provider, backend, err := llmresolver.Embed(ctx,
		resolverReq,
		runtimeStateResolution,
		llmresolver.Randomly,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.Embed(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			llmresolver.Randomly,
		)
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("embed: client resolution failed: %w", err)
	}
	defer safeClose(client)

	// Envelope allowlist. It bounds embeddings too — the allowlist is TOTAL, not
	// per-kind, because an embedding call spends the mission's compute like any
	// other (see ResolutionBounds).
	if err := e.enforceResolutionBounds(ctx, "embed", provider, backend); err != nil {
		return nil, Meta{}, err
	}

	embeddings, err := client.Embed(ctx, prompt)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("embedding generation failed: %w", err)
	}

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
	}
	return embeddings, meta, nil
}

func (e *modelManager) Stream(
	ctx context.Context,
	req Request,
	messages []libmodelprovider.Message,
	opts ...libmodelprovider.ChatArgument,
) (<-chan *libmodelprovider.StreamParcel, Meta, error) {
	if len(messages) == 0 {
		return nil, Meta{}, errors.New("messages cannot be empty")
	}

	if err := validateRequest(req); err != nil {
		return nil, Meta{}, fmt.Errorf("invalid request: %w", err)
	}

	runtimeStateResolution := e.GetRuntime(ctx)

	// Apply defaults if not provided
	if len(req.ModelNames) == 0 && e.config.DefaultChatModel.Name != "" {
		req.ModelNames = []string{e.config.DefaultChatModel.Name}
	}
	if len(req.ProviderTypes) == 0 && e.config.DefaultChatModel.Provider != "" {
		req.ProviderTypes = []string{e.config.DefaultChatModel.Provider}
	}

	resolverReq := e.convertToResolverRequest(req, messages)
	// Session-sticky resolution (blueprint §4.1.5); empty key == Randomly.
	sessionKey := effectiveSessionKey(ctx, req)
	policy := llmresolver.StickyOrRandom(sessionKey)
	client, provider, backend, err := llmresolver.Stream(ctx,
		resolverReq,
		runtimeStateResolution,
		policy,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.Stream(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			policy,
		)
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("stream: client resolution failed: %w", err)
	}

	// Envelope allowlist, checked before the stream is opened (see bounds.go).
	if err := e.enforceResolutionBounds(ctx, "stream", provider, backend); err != nil {
		safeClose(client)
		return nil, Meta{}, err
	}

	stream, err := client.Stream(ctx, messages, withCanonicalRequestShape(opts, providerCacheHints(req.CacheHints, sessionKey))...)
	if err != nil {
		safeClose(client)
		return nil, Meta{}, fmt.Errorf("stream initialization failed: %w", err)
	}

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
	}

	// Wrap the stream to close the client when done. Every relay send selects
	// on ctx.Done() so an abandoned consumer can never strand this goroutine
	// (and the client it holds) on a blocked channel send. The relay also
	// observes provider usage reports (mid-stream and terminal parcels) and
	// records them in the tracker once per request, so token accounting exists
	// even for callers that ignore the parcels themselves.
	wrappedStream := make(chan *libmodelprovider.StreamParcel)
	go func() {
		defer close(wrappedStream)
		defer safeClose(client)

		var usage libmodelprovider.TokenUsage
		sawUsage := false
		for parcel := range stream {
			if parcel.Usage != nil {
				mergeTokenUsage(&usage, parcel.Usage)
				sawUsage = true
			}
			if parcel.Terminal != nil && parcel.Terminal.Usage != nil {
				mergeTokenUsage(&usage, parcel.Terminal.Usage)
				sawUsage = true
			}
			select {
			case wrappedStream <- parcel:
			case <-ctx.Done():
				if sawUsage {
					e.reportTokenUsage(ctx, req, meta, usage)
				}
				return
			}
			if parcel.Error != nil {
				break
			}
		}
		if sawUsage {
			e.reportTokenUsage(ctx, req, meta, usage)
		}
	}()

	return wrappedStream, meta, nil
}

// mergeTokenUsage merges a later provider usage report over an earlier one
// field-wise: zero means "not reported" (per the TokenUsage contract), so a
// provider that reports prompt tokens at stream start and completion tokens
// at the end accumulates into one complete record.
func mergeTokenUsage(dst *libmodelprovider.TokenUsage, src *libmodelprovider.TokenUsage) {
	if src.PromptTokens != 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens != 0 {
		dst.CompletionTokens = src.CompletionTokens
	}
	if src.TotalTokens != 0 {
		dst.TotalTokens = src.TotalTokens
	}
	if src.CacheReadTokens != 0 {
		dst.CacheReadTokens = src.CacheReadTokens
	}
	if src.CacheWriteTokens != 0 {
		dst.CacheWriteTokens = src.CacheWriteTokens
	}
}

// reportTokenUsage records one request's provider-reported token accounting
// in the tracker. This is the measurement seam for cache utilization
// (blueprint §4.4): once modelrepo.TokenUsage grows cache-read/cache-write
// counters, they ride this same event and warm/cold hit rates become
// observable without touching any caller.
func (e *modelManager) reportTokenUsage(ctx context.Context, req Request, meta Meta, usage libmodelprovider.TokenUsage) {
	tracker := req.Tracker
	if tracker == nil {
		tracker = e.tracker
	}
	if tracker == nil {
		return
	}
	// The stream often ends because the consumer's context was canceled or
	// completed; the usage record must still land.
	ctx = context.WithoutCancel(ctx)
	_, reportChange, end := tracker.Start(ctx, "report", "token_usage",
		"model", meta.ModelName,
		"provider_type", meta.ProviderType,
		"backend_id", meta.BackendID,
	)
	defer end()
	reportChange("token_usage", map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		// Cache accounting (blueprint §4.4): prompt_tokens is the TOTAL prompt
		// count on every provider, so the warm hit rate of a request is
		// cache_read / (prompt_tokens - cache_write).
		"cache_read":  usage.CacheReadTokens,
		"cache_write": usage.CacheWriteTokens,
	})
}

func (e *modelManager) GetRuntime(ctx context.Context) runtimestate.ProviderFromRuntimeState {
	state := e.runtime.Get(ctx)
	return runtimestate.LocalProviderAdapter(ctx, e.tracker, state)
}

func (e *modelManager) GetTokenizer(ctx context.Context, modelName string) (Tokenizer, error) {
	if e.tokenizer == nil {
		return nil, errors.New("tokenizer not initialized")
	}

	// Get the optimal model for tokenization
	modelForTokenization, err := e.tokenizer.OptimalModel(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("failed to get optimal tokenizer model: %w", err)
	}

	// Return an adapter that uses the optimal model
	return &tokenizerAdapter{
		tokenizer: e.tokenizer,
		modelName: modelForTokenization,
	}, nil
}

// convertToResolverRequest builds the resolver request, deriving the vision
// requirement from the messages so callers cannot forget to set it: any image
// attachment restricts resolution to vision-capable providers.
func (e *modelManager) convertToResolverRequest(req Request, messages []libmodelprovider.Message) llmresolver.Request {
	return llmresolver.Request{
		ProviderTypes:  req.ProviderTypes,
		ModelNames:     req.ModelNames,
		ContextLength:  req.ContextLength,
		RequiresVision: libmodelprovider.MessagesHaveImages(messages),
		Tracker:        req.Tracker,
	}
}

func (e *modelManager) convertToResolverEmbedRequest(req EmbedRequest) llmresolver.EmbedRequest {
	return llmresolver.EmbedRequest{
		ModelName:    req.ModelName,
		ProviderType: req.ProviderType,
		Tracker:      req.Tracker,
	}
}

func validateRequest(req Request) error {
	if req.ContextLength < 0 {
		return errors.New("context length must be non-negative")
	}
	return nil
}

func safeClose(closer interface{}) {
	if closer == nil {
		return
	}

	// Type switch for different client types that might have Close methods
	switch c := closer.(type) {
	case interface{ Close() error }:
		_ = c.Close()
	case interface{ Close() }:
		c.Close()
	}
}

type tokenizerAdapter struct {
	tokenizer ollamatokenizer.Tokenizer
	modelName string
}

func (a *tokenizerAdapter) Tokenize(ctx context.Context, prompt string) ([]int, error) {
	return a.tokenizer.Tokenize(ctx, a.modelName, prompt)
}

func (a *tokenizerAdapter) CountTokens(ctx context.Context, prompt string) (int, error) {
	return a.tokenizer.CountTokens(ctx, a.modelName, prompt)
}
