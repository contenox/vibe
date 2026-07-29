//go:build llamanode

package llamasession

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
)

// phiModelPath returns the phi-4-mini GGUF path or skips. phi-4-mini's chat
// template is not prefix-stable (a system-only render gets an end-of-text marker
// appended), which is the B-002 reproduction vehicle. It is intentionally not
// size-limited like requireTinyGGUF.
func phiModelPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CONTENOX_LLAMA_PHI4MINI_GGUF")
	if p == "" {
		t.Skip("set CONTENOX_LLAMA_PHI4MINI_GGUF to the phi-4-mini GGUF to run this test")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// splitManifest builds a system(stable)+user(volatile) manifest for arbitrary content.
func splitManifest(stable, suffix string) llama.ContextManifest {
	return llama.ContextManifest{
		ProfileID:            "phi-test",
		Backend:              "llamacpp",
		BackendVersion:       "test",
		ModelDigest:          "phi",
		PromptFormat:         "chatml",
		PromptTemplateDigest: "test-template",
		RuntimeDigest:        "test-runtime",
		AddBOS:               false,
		StableBytes:          len(stable),
		TotalBytes:           len(stable) + len(suffix),
		StableByteHash:       shaHex(stable),
		Segments: []llama.ManifestSegment{
			{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: shaHex(stable)},
			{Kind: "user", Stable: false, ByteStart: len(stable), ByteEnd: len(stable) + len(suffix), ByteHash: shaHex(suffix)},
		},
	}
}

func drainDecode(t *testing.T, sess llama.Session, ctx context.Context, maxTokens int) string {
	t.Helper()
	temp := 0.0
	seed := 7
	chunks, err := sess.Decode(ctx, llama.DecodeConfig{MaxTokens: maxTokens, Temperature: &temp, Seed: &seed})
	if err != nil {
		t.Fatalf("decode start: %v", err)
	}
	var out strings.Builder
	for c := range chunks {
		if c.Error != nil {
			t.Fatalf("decode: %v", c.Error)
		}
		out.WriteString(c.Text)
	}
	return out.String()
}

// TestSystem_LlamaSession_Phi4Mini_PrefixStable is the B-002 end-to-end guard: a
// non-prefix-stable template must still complete multi-turn chat. PrefillSuffix
// reconciles against the resident tape at the token level instead of byte-matching
// a separately rendered stable prefix, so the stray end-of-text marker phi-4-mini
// appends to a system-only render is diffed away rather than fatally rejected.
func TestSystem_LlamaSession_Phi4Mini_PrefixStable(t *testing.T) {
	modelPath := phiModelPath(t)

	sess, err := New(modelPath, llama.Config{NumCtx: 2048, NumBatch: 64, NumThreads: 4, DisableBOS: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	stable := "You are a helpful assistant."

	// Turn 1.
	suffix1 := "Say the word hello and nothing else."
	m1 := splitManifest(stable, suffix1)
	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m1}); err != nil {
		t.Fatalf("turn1 EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix1, Manifest: m1}); err != nil {
		t.Fatalf("turn1 PrefillSuffix (B-002): %v", err)
	}
	out1 := drainDecode(t, sess, ctx, 8)
	t.Logf("turn1 output: %q", out1)

	// Turn 2: a second user message on the same session (same stable system).
	suffix2 := "Now say the word world."
	m2 := splitManifest(stable, suffix2)
	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m2}); err != nil {
		t.Fatalf("turn2 EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix2, Manifest: m2}); err != nil {
		t.Fatalf("turn2 PrefillSuffix (B-002): %v", err)
	}
	out2 := drainDecode(t, sess, ctx, 8)
	t.Logf("turn2 output: %q", out2)
}
