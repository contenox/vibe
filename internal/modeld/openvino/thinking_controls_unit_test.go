package openvino

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// TestUnit_OpenvinoThinkingControlsReachTemplate pins that explicit
// thinking/effort controls are honored, not silently dropped: they travel to
// the model's chat template as extra context on the generation-prompt render
// (parity with the llama backend's renderTemplateForDecode kwargs), and
// nil/empty means backend default with no extra context sent.
func TestUnit_OpenvinoThinkingControlsReachTemplate(t *testing.T) {
	ctx := context.Background()
	fake := &fakeGenAIBackend{}
	s := newGenaiSession(fake, 4096)

	fullText := "rulesask"
	stable := "rules"
	m := ovManifest(contextasm.HashString(stable), "r1")
	m.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: 5, ByteHash: contextasm.HashString("rules")},
		{Kind: "user", ByteStart: 5, ByteEnd: len(fullText), ByteHash: contextasm.HashString("ask")},
	}
	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}

	enable := true
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: "ask", Manifest: m, EnableThinking: &enable, ReasoningEffort: "high"}); err != nil {
		t.Fatalf("PrefillSuffix with thinking controls: %v", err)
	}
	if len(fake.templateEnableThinking) == 0 {
		t.Fatal("thinking controls never reached the chat template")
	}
	last := len(fake.templateEnableThinking) - 1
	if !fake.templateAddGenerationPrompt[last] {
		t.Fatalf("thinking controls must ride the generation-prompt render")
	}
	if got := fake.templateEnableThinking[last]; got == nil || !*got {
		t.Fatalf("enable_thinking = %v, want true", got)
	}
	if got := fake.templateReasoningEffort[last]; got != "high" {
		t.Fatalf("reasoning_effort = %q, want high", got)
	}

	// A default render (no explicit controls) sends no extra context.
	fake2 := &fakeGenAIBackend{}
	s2 := newGenaiSession(fake2, 4096)
	if _, err := s2.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s2.PrefillSuffix(ctx, transport.SuffixInput{Text: "ask", Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix without controls: %v", err)
	}
	for i, et := range fake2.templateEnableThinking {
		if et != nil || fake2.templateReasoningEffort[i] != "" {
			t.Fatalf("default render %d sent extra context: enable=%v effort=%q", i, et, fake2.templateReasoningEffort[i])
		}
	}
}
