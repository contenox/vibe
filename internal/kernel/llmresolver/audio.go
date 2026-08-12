package llmresolver

import (
	"context"
	"errors"
	"fmt"

	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

// ErrNoAudioCapableModel is returned when an audio-bearing request has no
// audio-capable candidate; it wraps ErrNoSatisfactoryModel.
var ErrNoAudioCapableModel = fmt.Errorf("%w: no available model supports audio input", ErrNoSatisfactoryModel)

// ErrPinnedModelLacksAudio is returned when an audio-bearing request pins a
// model that cannot accept audio; it does not wrap ErrNoSatisfactoryModel.
var ErrPinnedModelLacksAudio = errors.New("explicitly requested model lacks audio capability")

func audioCapCheck(req Request, base func(libmodelprovider.Provider) bool) func(libmodelprovider.Provider) bool {
	if !req.RequiresAudio {
		return base
	}
	return func(p libmodelprovider.Provider) bool {
		return base(p) && p.CanAudio()
	}
}

func refusePinnedNonAudio(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	base func(libmodelprovider.Provider) bool,
) error {
	if !req.RequiresAudio || len(req.ModelNames) == 0 {
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
			if p.CanAudio() {
				return nil
			}
		}
		if matched {
			return fmt.Errorf("%w: requested model %q does not accept audio and the request carries audio attachments; name an audio-capable model, set an audio-capable default (default-audio-model), or drop the model pin to let routing pick one",
				ErrPinnedModelLacksAudio, name)
		}
	}
	return nil
}

func classifyAudioFailure(
	ctx context.Context,
	req Request,
	getModels func(ctx context.Context, backendTypes ...string) ([]libmodelprovider.Provider, error),
	base func(libmodelprovider.Provider) bool,
	err error,
) error {
	if !req.RequiresAudio || !errors.Is(err, ErrNoSatisfactoryModel) || errors.Is(err, ErrNoVisionCapableModel) {
		return err
	}
	nonAudioCandidates, filterErr := filterCandidates(ctx, req, getModels, base)
	if filterErr != nil || len(nonAudioCandidates) == 0 {
		return err
	}
	names := make([]string, 0, len(nonAudioCandidates))
	for _, p := range nonAudioCandidates {
		names = append(names, p.ModelName())
	}
	return fmt.Errorf("%w: matching models %v do not accept audio; use an audio-capable model for requests with audio attachments", ErrNoAudioCapableModel, names)
}
