//go:build openvino && openvino_genai

package openvino

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// TestSystem_OpenVINO_PrefillSuffixOverflowParksToCold proves the residency
// driver on the real OpenVINO GenAI backend: a turn that overflows the hot window
// parks the resident volatile context to the cold store (recoverable) instead of
// only letting native sink+recent eviction drop it. Unlike llama, OpenVINO runs
// native eviction so an overflow does not error; the assertion is that cold KV
// was actually created.
func TestSystem_OpenVINO_PrefillSuffixOverflowParksToCold(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL (see Makefile.openvino)")
	}
	const numCtx = 64
	stable := "system\nYou are concise.\n"
	bigTurn := "user\nalpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon.\n"
	smallTurn := "user\none two three four five six seven eight nine ten eleven twelve.\n"

	mani := func(s, suf string) contextasm.ContextManifest {
		return contextasm.ContextManifest{
			Backend:              "openvino",
			ModelDigest:          "test-model",
			PromptFormat:         "openvino_chat_template",
			PromptTemplateDigest: "test-model",
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sess, err := NewService().OpenSession(ctx, transport.OpenSessionRequest{
		ModelName: "test", Type: "openvino", Path: modelDir,
		Config: transport.Config{NumCtx: numCtx, PlannerEffectiveContext: 384},
	})
	if err != nil {
		t.Fatalf("OpenSession on %s: %v", resolveDevice(), err)
	}
	defer sess.Close()

	gs := sess.(*genaiSession)
	if !gs.coldEnabledLocked() {
		t.Skip("cold store not enabled for this model (kv precision / cold KV support)")
	}

	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: mani(stable, bigTurn)}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: bigTurn, Manifest: mani(stable, bigTurn)}); err != nil {
		t.Fatalf("PrefillSuffix(big): %v", err)
	}
	t.Logf("resident after big = %d (numCtx=%d)", gs.residentTokens(), numCtx)

	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: smallTurn, Manifest: mani(stable, smallTurn)}); err != nil {
		t.Fatalf("PrefillSuffix(small): %v", err)
	}
	if gs.residentTokens() > numCtx {
		t.Fatalf("hot resident %d exceeded numCtx %d", gs.residentTokens(), numCtx)
	}
	if gs.coldTokens == 0 {
		t.Fatal("expected volatile context parked to cold after overflow, got coldTokens=0")
	}
	t.Logf("parked %d tokens to cold; hot resident = %d", gs.coldTokens, gs.residentTokens())
}
