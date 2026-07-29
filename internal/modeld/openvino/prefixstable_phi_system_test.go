//go:build openvino && openvino_genai

package openvino

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// TestSystem_OpenVINO_Phi4Mini_PrefixStable is the OpenVINO half of the B-002
// guard: phi-4-mini's chat template is not prefix-stable (a system-only render is
// decorated differently than the head of the full render), which byte-matching a
// separately rendered stable prefix fatally rejected. PrefillSuffix now reconciles
// against the resident tape at the token level, so a non-prefix-stable template
// completes multi-turn chat instead of erroring "not prefix-stable".
func TestSystem_OpenVINO_Phi4Mini_PrefixStable(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_PHI4MINI_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_PHI4MINI_MODEL to the phi-4-mini-ov IR directory")
	}

	mani := func(s, suf string) contextasm.ContextManifest {
		return contextasm.ContextManifest{
			Backend:              "openvino",
			ModelDigest:          "phi-4-mini-ov",
			PromptFormat:         "openvino_chat_template",
			PromptTemplateDigest: "phi-4-mini-ov",
			RuntimeDigest:        resolveDevice(),
			StableByteHash:       contextasm.HashString(s),
			StableBytes:          len(s),
			TotalBytes:           len(s) + len(suf),
			Segments: []contextasm.ManifestSegment{
				{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(s), ByteHash: contextasm.HashString(s)},
				{Kind: "user", Stable: false, ByteStart: len(s), ByteEnd: len(s) + len(suf), ByteHash: contextasm.HashString(suf)},
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	sess, err := NewService().OpenSession(ctx, transport.OpenSessionRequest{
		ModelName: "phi-4-mini-ov", Type: "openvino", Path: modelDir,
		Config: transport.Config{NumCtx: 2048},
	})
	if err != nil {
		t.Fatalf("OpenSession on %s: %v", resolveDevice(), err)
	}
	defer sess.Close()

	decode := func() string {
		ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: 8})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		var b strings.Builder
		for c := range ch {
			if c.Error != nil {
				t.Fatalf("decode chunk: %v", c.Error)
			}
			b.WriteString(c.Text)
		}
		return b.String()
	}

	stable := "You are a helpful assistant."

	// Turn 1.
	suf1 := "Say the word hello and nothing else."
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: mani(stable, suf1)}); err != nil {
		t.Fatalf("turn1 EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: suf1, Manifest: mani(stable, suf1)}); err != nil {
		t.Fatalf("turn1 PrefillSuffix (B-002): %v", err)
	}
	t.Logf("turn1 output: %q", decode())

	// Turn 2: a second user message on the same session (same stable system).
	suf2 := "Now say the word world."
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: mani(stable, suf2)}); err != nil {
		t.Fatalf("turn2 EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: suf2, Manifest: mani(stable, suf2)}); err != nil {
		t.Fatalf("turn2 PrefillSuffix (B-002): %v", err)
	}
	t.Logf("turn2 output: %q", decode())
}
