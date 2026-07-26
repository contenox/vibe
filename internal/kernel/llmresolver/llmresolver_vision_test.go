package llmresolver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/llmresolver"
	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
)

// TestUnit_ChatModelResolution_VisionGate covers the vision capability gate:
// requests carrying images resolve only to CanVision providers, and when none
// exists the failure is the typed vision error — never a silent fallback to a
// text-only model.
func TestUnit_ChatModelResolution_VisionGate(t *testing.T) {
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

	t.Run("image request selects the vision-capable provider", func(t *testing.T) {
		_, provider, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{RequiresVision: true},
			getModels([]libmodelprovider.Provider{textOnly, visionCapable}),
			llmresolver.Randomly,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if provider.GetID() != "vision" {
			t.Errorf("expected vision-capable provider, got %s", provider.GetID())
		}
	})

	t.Run("image request with only text models fails with the typed vision error", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{RequiresVision: true},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrNoVisionCapableModel) {
			t.Fatalf("want ErrNoVisionCapableModel, got %v", err)
		}
		// The typed error still wraps the generic no-match error so existing
		// handling (e.g. the resolution self-heal cycle) keeps firing.
		if !errors.Is(err, llmresolver.ErrNoSatisfactoryModel) {
			t.Errorf("vision error must wrap ErrNoSatisfactoryModel, got %v", err)
		}
		if !strings.Contains(err.Error(), "qwen3-4b") {
			t.Errorf("error should name the text-only candidates:\n%s", err.Error())
		}
	})

	t.Run("stream path enforces the same gate", func(t *testing.T) {
		_, _, _, err := llmresolver.Stream(context.Background(),
			llmresolver.Request{RequiresVision: true},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrNoVisionCapableModel) {
			t.Fatalf("want ErrNoVisionCapableModel, got %v", err)
		}
	})

	t.Run("text request still resolves to a text-only provider", func(t *testing.T) {
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

	t.Run("no candidates at all keeps the generic error", func(t *testing.T) {
		_, _, _, err := llmresolver.Chat(context.Background(),
			llmresolver.Request{ModelNames: []string{"does-not-exist"}, RequiresVision: true},
			getModels([]libmodelprovider.Provider{textOnly}),
			llmresolver.Randomly,
		)
		if !errors.Is(err, llmresolver.ErrNoSatisfactoryModel) {
			t.Fatalf("want ErrNoSatisfactoryModel, got %v", err)
		}
		if errors.Is(err, llmresolver.ErrNoVisionCapableModel) {
			t.Errorf("a name mismatch is not a vision shortfall:\n%v", err)
		}
	})
}
