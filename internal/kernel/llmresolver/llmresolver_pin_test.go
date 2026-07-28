package llmresolver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

// TestUnit_ChatModelResolution_PinnedNonVisionRefuses pins that an explicit
// model pin combined with an image request refuses with a teaching error
// when the pinned model lacks vision, rather than silently resolving to a
// different vision-capable model; unpinned requests keep auto-selecting.
func TestUnit_ChatModelResolution_PinnedNonVisionRefuses(t *testing.T) {
	getModels := func(providers []libmodelprovider.Provider) func(context.Context, ...string) ([]libmodelprovider.Provider, error) {
		return func(context.Context, ...string) ([]libmodelprovider.Provider, error) { return providers, nil }
	}

	textOnly := &libmodelprovider.MockProvider{
		ID: "text", Name: "qwen3-4b", ContextLength: 8192,
		CanChatFlag: true, CanStreamFlag: true, Backends: []string{"b1"},
	}
	visionCapable := &libmodelprovider.MockProvider{
		ID: "vision", Name: "gemma3-vlm", ContextLength: 8192,
		CanChatFlag: true, CanStreamFlag: true, CanVisionFlag: true, Backends: []string{"b2"},
	}
	all := []libmodelprovider.Provider{textOnly, visionCapable}

	t.Run("pinned text-only model with images refuses instead of swapping", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"qwen3-4b"}, RequiresVision: true},
			getModels(all),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrPinnedModelLacksVision) {
			t.Fatalf("want ErrPinnedModelLacksVision, got %v", err)
		}
		if !strings.Contains(err.Error(), "qwen3-4b") {
			t.Errorf("refusal must name the pinned model:\n%s", err.Error())
		}
		if !strings.Contains(err.Error(), "drop the model pin") {
			t.Errorf("refusal must teach the remedy:\n%s", err.Error())
		}
		if errors.Is(err, llmresolver.ErrNoSatisfactoryModel) {
			t.Error("a deterministic pin refusal must not trigger no-match retry cycles")
		}
	})

	t.Run("pin plus fallback names never silently skip the pin", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"qwen3-4b", "gemma3-vlm"}, RequiresVision: true},
			getModels(all),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrPinnedModelLacksVision) {
			t.Fatalf("want ErrPinnedModelLacksVision, got %v", err)
		}
	})

	t.Run("pinned vision-capable model resolves", func(t *testing.T) {
		_, provider, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"gemma3-vlm"}, RequiresVision: true},
			getModels(all),
			llmresolver.Randomly,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider.GetID() != "vision" {
			t.Errorf("expected the pinned vision model, got %s", provider.GetID())
		}
	})

	t.Run("no pin keeps capability-based auto-selection", func(t *testing.T) {
		_, provider, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{RequiresVision: true},
			getModels(all),
			llmresolver.Randomly,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider.GetID() != "vision" {
			t.Errorf("expected auto-selected vision model, got %s", provider.GetID())
		}
	})

	t.Run("pin that matches nothing keeps the generic no-match error", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"does-not-exist"}, RequiresVision: true},
			getModels(all),
			llmresolver.Randomly,
		)
		if errors.Is(err, llmresolver.ErrPinnedModelLacksVision) {
			t.Fatalf("a name mismatch is not a pin refusal: %v", err)
		}
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("stream path refuses the same way", func(t *testing.T) {
		_, _, _, err := llmresolver.Stream(context.Background(),
			llmresolver.Request{ModelNames: []string{"qwen3-4b"}, RequiresVision: true},
			getModels(all),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrPinnedModelLacksVision) {
			t.Fatalf("want ErrPinnedModelLacksVision on stream, got %v", err)
		}
	})

	t.Run("text-only pin without images still resolves", func(t *testing.T) {
		_, provider, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"qwen3-4b"}},
			getModels(all),
			llmresolver.Randomly,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider.GetID() != "text" {
			t.Errorf("expected the pinned text model, got %s", provider.GetID())
		}
	})
}
