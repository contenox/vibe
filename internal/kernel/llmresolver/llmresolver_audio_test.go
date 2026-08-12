package llmresolver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

// TestUnit_ChatModelResolution_AudioGate covers the audio capability gate:
// audio requests resolve only to CanAudio providers or fail with a typed error.
func TestUnit_ChatModelResolution_AudioGate(t *testing.T) {
	getModels := func(providers []libmodelprovider.Provider) func(context.Context, ...string) ([]libmodelprovider.Provider, error) {
		return func(context.Context, ...string) ([]libmodelprovider.Provider, error) { return providers, nil }
	}

	textOnly := &libmodelprovider.MockProvider{
		ID: "text", Name: "qwen3-4b", ContextLength: 8192,
		CanChatFlag: true, CanStreamFlag: true, Backends: []string{"b1"},
	}
	audioCapable := &libmodelprovider.MockProvider{
		ID: "audio", Name: "gemini-2-5-flash", ContextLength: 8192,
		CanChatFlag: true, CanStreamFlag: true, CanAudioFlag: true, Backends: []string{"b2"},
	}

	t.Run("audio request selects the audio-capable provider", func(t *testing.T) {
		_, provider, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{RequiresAudio: true},
			getModels([]libmodelprovider.Provider{textOnly, audioCapable}),
			llmresolver.Randomly,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider.GetID() != "audio" {
			t.Errorf("expected audio-capable provider, got %s", provider.GetID())
		}
	})

	t.Run("audio request with no audio-capable model fails with the typed audio error", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{RequiresAudio: true},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrNoAudioCapableModel) {
			t.Fatalf("want ErrNoAudioCapableModel, got %v", err)
		}
		if !errors.Is(err, llmresolver.ErrNoSatisfactoryModel) {
			t.Errorf("audio error must wrap ErrNoSatisfactoryModel, got %v", err)
		}
		if !strings.Contains(err.Error(), "qwen3-4b") {
			t.Errorf("error should name the non-audio candidates:\n%s", err.Error())
		}
	})

	t.Run("stream path enforces the same gate", func(t *testing.T) {
		_, _, _, err := llmresolver.Stream(context.Background(),
			llmresolver.Request{RequiresAudio: true},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrNoAudioCapableModel) {
			t.Fatalf("want ErrNoAudioCapableModel, got %v", err)
		}
	})

	t.Run("text request still resolves to a non-audio provider", func(t *testing.T) {
		_, provider, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"qwen3-4b"}},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider.GetID() != "text" {
			t.Errorf("expected text provider, got %s", provider.GetID())
		}
	})

	t.Run("pinned non-audio model refuses instead of swapping", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"qwen3-4b", "gemini-2-5-flash"}, RequiresAudio: true},
			getModels([]libmodelprovider.Provider{textOnly, audioCapable}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrPinnedModelLacksAudio) {
			t.Fatalf("want ErrPinnedModelLacksAudio, got %v", err)
		}
		if errors.Is(err, llmresolver.ErrNoSatisfactoryModel) {
			t.Errorf("pinned refusal must not wrap ErrNoSatisfactoryModel:\n%v", err)
		}
	})

	t.Run("no candidates at all keeps the generic error", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"does-not-exist"}, RequiresAudio: true},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrNoSatisfactoryModel) {
			t.Fatalf("want ErrNoSatisfactoryModel, got %v", err)
		}
		if errors.Is(err, llmresolver.ErrNoAudioCapableModel) {
			t.Errorf("a name mismatch is not an audio shortfall:\n%v", err)
		}
	})
}
