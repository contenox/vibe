package llmrepo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/ollamatokenizer"
	"github.com/contenox/contenox/internal/models/runtimestate"
	"github.com/contenox/contenox/libtracker"
)

var _ ModelRepo = (*modelManager)(nil)

// Request is the unified request type for all operations.
type Request struct {
	ProviderTypes []string // Optional: if empty, uses all default providers
	ModelNames    []string // Optional: if empty, any model is considered
	ContextLength int      // Minimum required context length
	// SessionKey is an opaque per-session cache-affinity key (derive it with
	// DeriveSessionKey); non-empty pins the session to one provider/backend.
	SessionKey string
	// CacheHints declares where the stable/volatile boundary of this request
	// lies; nil changes nothing.
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
	// operation carries no other channel for it (PromptExecute); nil means unreported.
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

	reconcileMu     sync.Mutex
	lastReconcileAt time.Time
}

const minResolveReconcileInterval = 5 * time.Second

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
	// DefaultAudioModel is the model preferred for audio-bearing chat/stream
	// requests that name no model themselves; unset falls back to DefaultChatModel.
	DefaultAudioModel ModelConfig
}

func (e *modelManager) applyAudioDefault(req *Request, messages []libmodelprovider.Message) {
	if !libmodelprovider.MessagesHaveAudio(messages) {
		return
	}
	if len(req.ModelNames) == 0 && e.config.DefaultAudioModel.Name != "" {
		req.ModelNames = []string{e.config.DefaultAudioModel.Name}
		if len(req.ProviderTypes) == 0 && e.config.DefaultAudioModel.Provider != "" {
			req.ProviderTypes = []string{e.config.DefaultAudioModel.Provider}
		}
	}
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

	if len(req.ModelNames) == 0 {
		req.ModelNames = []string{e.config.DefaultPromptModel.Name}
	}
	if len(req.ProviderTypes) == 0 {
		req.ProviderTypes = []string{e.config.DefaultPromptModel.Provider}
	}

	resolverReq := e.convertToResolverRequest(req, nil)
	// Session-sticky resolution: a session key pins to one provider/backend so
	// its prefix cache stays warm; empty key == Randomly.
	policy := llmresolver.StickyOrRandom(effectiveSessionKey(ctx, req))
	selections, err := llmresolver.PromptExecuteSelections(ctx,
		resolverReq,
		runtimeStateResolution,
		policy,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		selections, err = llmresolver.PromptExecuteSelections(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			policy,
		)
	}
	if err != nil {
		return "", Meta{}, fmt.Errorf("prompt execute: client resolution failed: %w", err)
	}

	// Bounds enforcement and the provider call run per selection: a backend
	// whose refusal is terminal for it alone is excluded and the next capable
	// backend tried (see failover.go).
	return e.runPromptSelections(ctx, req, selections, systemInstruction, temperature, prompt)
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

	// The audio role outranks the chat default for audio-bearing requests;
	// explicit model names outrank both.
	e.applyAudioDefault(&req, messages)
	if len(req.ModelNames) == 0 {
		req.ModelNames = []string{e.config.DefaultChatModel.Name}
	}
	if len(req.ProviderTypes) == 0 {
		req.ProviderTypes = []string{e.config.DefaultChatModel.Provider}
	}

	resolverReq := e.convertToResolverRequest(req, messages)
	// Session-sticky resolution; empty key == Randomly.
	sessionKey := effectiveSessionKey(ctx, req)
	policy := llmresolver.StickyOrRandom(sessionKey)
	selections, err := llmresolver.ChatSelections(ctx,
		resolverReq,
		runtimeStateResolution,
		policy,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		selections, err = llmresolver.ChatSelections(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			policy,
		)
	}
	if err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat: client resolution failed: %w", err)
	}

	// Bounds enforcement and the provider call run per selection: a backend
	// whose refusal is terminal for it alone is excluded and the next capable
	// backend tried (see failover.go).
	return e.runChatSelections(ctx, req, selections, sessionKey, messages, opts)
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

	if embedReq.ModelName == "" {
		embedReq.ModelName = e.config.DefaultEmbeddingModel.Name
	}
	if embedReq.ProviderType == "" {
		embedReq.ProviderType = e.config.DefaultEmbeddingModel.Provider
	}

	resolverReq := e.convertToResolverEmbedRequest(embedReq)
	// Embedding stays on Randomly deliberately: EmbedRequest carries no
	// session identity and embeddings gain nothing from prefix caches.
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

	// Envelope allowlist bounds embeddings too: the allowlist is TOTAL, not
	// per-kind (see ResolutionBounds).
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

	// The audio role outranks the chat default for audio-bearing requests;
	// explicit model names outrank both.
	e.applyAudioDefault(&req, messages)
	if len(req.ModelNames) == 0 && e.config.DefaultChatModel.Name != "" {
		req.ModelNames = []string{e.config.DefaultChatModel.Name}
	}
	if len(req.ProviderTypes) == 0 && e.config.DefaultChatModel.Provider != "" {
		req.ProviderTypes = []string{e.config.DefaultChatModel.Provider}
	}

	resolverReq := e.convertToResolverRequest(req, messages)
	// Session-sticky resolution; empty key == Randomly.
	sessionKey := effectiveSessionKey(ctx, req)
	policy := llmresolver.StickyOrRandom(sessionKey)
	selections, err := llmresolver.StreamSelections(ctx,
		resolverReq,
		runtimeStateResolution,
		policy,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		selections, err = llmresolver.StreamSelections(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			policy,
		)
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("stream: client resolution failed: %w", err)
	}

	// Bounds enforcement, stream setup, and the first-parcel failover peek run
	// per selection: a backend whose refusal is terminal for it alone is
	// excluded and the next capable backend tried (see failover.go).
	return e.runStreamSelections(ctx, req, selections, sessionKey, messages, opts)
}

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
		// Cache accounting: prompt_tokens is the TOTAL prompt count, so warm
		// hit rate is cache_read / (prompt_tokens - cache_write).
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

	modelForTokenization, err := e.tokenizer.OptimalModel(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("failed to get optimal tokenizer model: %w", err)
	}

	return &tokenizerAdapter{
		tokenizer: e.tokenizer,
		modelName: modelForTokenization,
	}, nil
}

func (e *modelManager) convertToResolverRequest(req Request, messages []libmodelprovider.Message) llmresolver.Request {
	return llmresolver.Request{
		ProviderTypes:  req.ProviderTypes,
		ModelNames:     req.ModelNames,
		ContextLength:  req.ContextLength,
		RequiresVision: libmodelprovider.MessagesHaveImages(messages),
		RequiresAudio:  libmodelprovider.MessagesHaveAudio(messages),
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
