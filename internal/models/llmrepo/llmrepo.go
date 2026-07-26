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
	Tracker       libtracker.ActivityTracker
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
// afterwards — most commonly modeld being (re)started after the runtime — stays
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
	client, provider, backend, err := llmresolver.PromptExecute(ctx,
		resolverReq,
		runtimeStateResolution,
		llmresolver.Randomly,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.PromptExecute(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			llmresolver.Randomly,
		)
	}
	if err != nil {
		return "", Meta{}, fmt.Errorf("prompt execute: client resolution failed: %w", err)
	}
	defer safeClose(client)

	result, err := client.Prompt(ctx, systemInstruction, temperature, prompt)
	if err != nil {
		return "", Meta{}, fmt.Errorf("prompt execution failed: %w", err)
	}

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
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
	client, provider, backend, err := llmresolver.Chat(ctx,
		resolverReq,
		runtimeStateResolution,
		llmresolver.Randomly,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.Chat(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			llmresolver.Randomly,
		)
	}
	if err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat: client resolution failed: %w", err)
	}
	defer safeClose(client)

	response, err := client.Chat(ctx, messages, opts...)
	if err != nil {
		return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat execution failed: %w", err)
	}

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
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
	client, provider, backend, err := llmresolver.Stream(ctx,
		resolverReq,
		runtimeStateResolution,
		llmresolver.Randomly,
	)
	if err != nil && e.reconcileForResolution(ctx, err) {
		client, provider, backend, err = llmresolver.Stream(ctx,
			resolverReq,
			e.GetRuntime(ctx),
			llmresolver.Randomly,
		)
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("stream: client resolution failed: %w", err)
	}

	stream, err := client.Stream(ctx, messages, opts...)
	if err != nil {
		safeClose(client)
		return nil, Meta{}, fmt.Errorf("stream initialization failed: %w", err)
	}

	// Wrap the stream to close the client when done
	wrappedStream := make(chan *libmodelprovider.StreamParcel)
	go func() {
		defer close(wrappedStream)
		defer safeClose(client)

		for parcel := range stream {
			wrappedStream <- parcel
			if parcel.Error != nil {
				break
			}
		}
	}()

	meta := Meta{
		ModelName:    provider.ModelName(),
		ProviderType: provider.GetType(),
		BackendID:    backend,
	}
	return wrappedStream, meta, nil
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
