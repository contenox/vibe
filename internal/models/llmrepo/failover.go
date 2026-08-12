package llmrepo

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

type backendRefusal struct {
	backend string
	model   string
	err     error
}

func exhaustedError(op string, refusals []backendRefusal) error {
	lines := make([]error, 0, len(refusals))
	for _, r := range refusals {
		lines = append(lines, fmt.Errorf("backend %s (model %s): %w", r.backend, r.model, r.err))
	}
	return fmt.Errorf("%s refused by every capable backend: %w", op, errors.Join(lines...))
}

func (e *modelManager) reportBackendRefusal(ctx context.Context, req Request, op string, sel llmresolver.Selection, refusalErr error) {
	tracker := req.Tracker
	if tracker == nil {
		tracker = e.tracker
	}
	if tracker == nil {
		return
	}
	_, reportChange, end := tracker.Start(ctx, "failover", op,
		"model", sel.Provider.ModelName(),
		"provider_type", sel.Provider.GetType(),
		"backend_id", sel.Backend,
	)
	defer end()
	reportChange("backend_excluded", map[string]any{
		"backend_id": sel.Backend,
		"model":      sel.Provider.ModelName(),
		"reason":     refusalErr.Error(),
	})
}

func skipOrFail(refusals *[]backendRefusal, sel llmresolver.Selection, err error) error {
	if len(*refusals) == 0 {
		return err
	}
	*refusals = append(*refusals, backendRefusal{backend: sel.Backend, model: sel.Provider.ModelName(), err: err})
	return nil
}

func (e *modelManager) runChatSelections(
	ctx context.Context,
	req Request,
	selections []llmresolver.Selection,
	sessionKey string,
	messages []libmodelprovider.Message,
	opts []libmodelprovider.ChatArgument,
) (libmodelprovider.ChatResult, Meta, error) {
	connCtx := libmodelprovider.WithRequestedContextLength(ctx, req.ContextLength)
	var refusals []backendRefusal
	for _, sel := range selections {
		provider, backend := sel.Provider, sel.Backend

		// Envelope allowlist, checked before anything is sent (see bounds.go).
		if err := e.enforceResolutionBounds(ctx, "chat", provider, backend); err != nil {
			if failErr := skipOrFail(&refusals, sel, err); failErr != nil {
				return libmodelprovider.ChatResult{}, Meta{}, failErr
			}
			continue
		}
		client, err := provider.GetChatConnection(connCtx, backend)
		if err != nil {
			if failErr := skipOrFail(&refusals, sel, err); failErr != nil {
				return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat: client resolution failed: %w", failErr)
			}
			continue
		}

		response, err := client.Chat(ctx, messages, withCanonicalRequestShape(opts, providerCacheHints(req.CacheHints, sessionKey))...)
		if err != nil {
			safeClose(client)
			if !libmodelprovider.IsBackendTerminal(err) {
				return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat execution failed: %w", err)
			}
			refusals = append(refusals, backendRefusal{backend: backend, model: provider.ModelName(), err: err})
			e.reportBackendRefusal(ctx, req, "chat", sel, err)
			continue
		}

		meta := Meta{
			ModelName:    provider.ModelName(),
			ProviderType: provider.GetType(),
			BackendID:    backend,
		}
		// Recorded on the same tracker event the stream path uses so cache hit
		// rates are observable on both paths.
		if response.Usage != nil {
			e.reportTokenUsage(ctx, req, meta, *response.Usage)
		}
		safeClose(client)
		return response, meta, nil
	}
	return libmodelprovider.ChatResult{}, Meta{}, fmt.Errorf("chat execution failed: %w", exhaustedError("chat", refusals))
}

func (e *modelManager) runPromptSelections(
	ctx context.Context,
	req Request,
	selections []llmresolver.Selection,
	systemInstruction string,
	temperature float32,
	prompt string,
) (string, Meta, error) {
	connCtx := libmodelprovider.WithRequestedContextLength(ctx, req.ContextLength)
	var refusals []backendRefusal
	for _, sel := range selections {
		provider, backend := sel.Provider, sel.Backend

		// Envelope allowlist (see bounds.go).
		if err := e.enforceResolutionBounds(ctx, "prompt", provider, backend); err != nil {
			if failErr := skipOrFail(&refusals, sel, err); failErr != nil {
				return "", Meta{}, failErr
			}
			continue
		}
		client, err := provider.GetPromptConnection(connCtx, backend)
		if err != nil {
			if failErr := skipOrFail(&refusals, sel, err); failErr != nil {
				return "", Meta{}, fmt.Errorf("prompt execute: client resolution failed: %w", failErr)
			}
			continue
		}

		result, usage, err := client.Prompt(ctx, systemInstruction, temperature, prompt)
		if err != nil {
			safeClose(client)
			if !libmodelprovider.IsBackendTerminal(err) {
				return "", Meta{}, fmt.Errorf("prompt execution failed: %w", err)
			}
			refusals = append(refusals, backendRefusal{backend: backend, model: provider.ModelName(), err: err})
			e.reportBackendRefusal(ctx, req, "prompt", sel, err)
			continue
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
		safeClose(client)
		return result, meta, nil
	}
	return "", Meta{}, fmt.Errorf("prompt execution failed: %w", exhaustedError("prompt", refusals))
}

func (e *modelManager) runStreamSelections(
	ctx context.Context,
	req Request,
	selections []llmresolver.Selection,
	sessionKey string,
	messages []libmodelprovider.Message,
	opts []libmodelprovider.ChatArgument,
) (<-chan *libmodelprovider.StreamParcel, Meta, error) {
	connCtx := libmodelprovider.WithRequestedContextLength(ctx, req.ContextLength)
	var refusals []backendRefusal
	for _, sel := range selections {
		provider, backend := sel.Provider, sel.Backend

		// Envelope allowlist, checked before the stream is opened (see bounds.go).
		if err := e.enforceResolutionBounds(ctx, "stream", provider, backend); err != nil {
			if failErr := skipOrFail(&refusals, sel, err); failErr != nil {
				return nil, Meta{}, failErr
			}
			continue
		}
		client, err := provider.GetStreamConnection(connCtx, backend)
		if err != nil {
			if failErr := skipOrFail(&refusals, sel, err); failErr != nil {
				return nil, Meta{}, fmt.Errorf("stream: client resolution failed: %w", failErr)
			}
			continue
		}

		stream, err := client.Stream(ctx, messages, withCanonicalRequestShape(opts, providerCacheHints(req.CacheHints, sessionKey))...)
		if err != nil {
			safeClose(client)
			if !libmodelprovider.IsBackendTerminal(err) {
				return nil, Meta{}, fmt.Errorf("stream initialization failed: %w", err)
			}
			refusals = append(refusals, backendRefusal{backend: backend, model: provider.ModelName(), err: err})
			e.reportBackendRefusal(ctx, req, "stream", sel, err)
			continue
		}

		// Peek the first parcel — the only failover-safe window.
		var first *libmodelprovider.StreamParcel
		select {
		case first = <-stream:
		case <-ctx.Done():
			safeClose(client)
			return nil, Meta{}, ctx.Err()
		}
		if first != nil && first.Error != nil && libmodelprovider.IsBackendTerminal(first.Error) {
			safeClose(client)
			refusals = append(refusals, backendRefusal{backend: backend, model: provider.ModelName(), err: first.Error})
			e.reportBackendRefusal(ctx, req, "stream", sel, first.Error)
			continue
		}

		meta := Meta{
			ModelName:    provider.ModelName(),
			ProviderType: provider.GetType(),
			BackendID:    backend,
		}
		return e.relayStream(ctx, req, meta, client, first, stream), meta, nil
	}
	return nil, Meta{}, fmt.Errorf("stream initialization failed: %w", exhaustedError("stream", refusals))
}

func (e *modelManager) relayStream(
	ctx context.Context,
	req Request,
	meta Meta,
	client any,
	first *libmodelprovider.StreamParcel,
	stream <-chan *libmodelprovider.StreamParcel,
) <-chan *libmodelprovider.StreamParcel {
	wrappedStream := make(chan *libmodelprovider.StreamParcel)
	go func() {
		defer close(wrappedStream)
		defer safeClose(client)

		var usage libmodelprovider.TokenUsage
		sawUsage := false
		finish := func() {
			if sawUsage {
				e.reportTokenUsage(ctx, req, meta, usage)
			}
		}
		forward := func(parcel *libmodelprovider.StreamParcel) bool {
			if parcel.Usage != nil {
				mergeTokenUsage(&usage, parcel.Usage)
				sawUsage = true
			}
			if parcel.Terminal != nil && parcel.Terminal.Usage != nil {
				mergeTokenUsage(&usage, parcel.Terminal.Usage)
				sawUsage = true
			}
			// Selecting on ctx.Done() here keeps an abandoned consumer from
			// stranding this goroutine on a blocked channel send.
			select {
			case wrappedStream <- parcel:
				return true
			case <-ctx.Done():
				return false
			}
		}

		if first != nil {
			if !forward(first) {
				finish()
				return
			}
			if first.Error != nil {
				finish()
				return
			}
		}
		for parcel := range stream {
			if !forward(parcel) {
				finish()
				return
			}
			if parcel.Error != nil {
				break
			}
		}
		finish()
	}()
	return wrappedStream
}
