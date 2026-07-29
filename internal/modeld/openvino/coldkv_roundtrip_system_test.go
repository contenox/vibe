//go:build openvino && openvino_genai

package openvino

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/internal/sessiontest"
	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// TestSystem_OpenVINOEvictAdmitTail_RestoresContinuation is the keystone E2E
// parity test (Phase 1 of effective-context-residency-loop-plan.md): it gives
// the real OpenVINO GenAI backend the same shared tail round-trip contract llama
// runs in llamasession.TestSystem_LlamaSessionEvictAdmitTail_RestoresContinuation.
//
// It exports a resident tail range to the host cold store via ExportColdKV, admits it
// back via ImportColdKV (the shifted f16 import with native RoPE rotation), and asserts
// greedy decoding is byte-identical across the round trip. Tail is the only
// order-preserving round trip today (admit re-imports at the resident tail); a middle
// round trip reorders and is Phase 3 (in-place reinsertion).
//
// Run: make -f Makefile.openvino test-genai
func TestSystem_OpenVINOEvictAdmitTail_RestoresContinuation(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL (see Makefile.openvino)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		numCtx     = 256
		plannerCtx = 384 // > numCtx: selects the cold store and sets coldMaxTokens > 0
		width      = 4
		decodeN    = 8
	)

	stable := "system\nYou are a concise assistant.\n"
	suffix := "user\nRepeat the final word from this list: alpha beta gamma delta epsilon.\n"
	manifest := contextasm.ContextManifest{
		Backend:              "openvino",
		ModelDigest:          "test-model",
		PromptFormat:         "openvino_chat_template",
		PromptTemplateDigest: "test-model",
		RuntimeDigest:        resolveDevice(),
		StableByteHash:       contextasm.HashString(stable),
	}

	sessiontest.RunTailColdRoundTrip(t, ctx, sessiontest.TailColdRoundTripCase{
		Open: func(t testing.TB) transport.Session {
			t.Helper()
			sess, err := NewService().OpenSession(ctx, transport.OpenSessionRequest{
				ModelName: "test",
				Type:      "openvino",
				Path:      modelDir,
				Config: transport.Config{
					NumCtx:                  numCtx,
					PlannerEffectiveContext: plannerCtx,
				},
			})
			if err != nil {
				t.Fatalf("OpenSession on %s: %v", resolveDevice(), err)
			}
			return sess
		},
		Build: func(t testing.TB, ctx context.Context, sess transport.Session) {
			t.Helper()
			if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: manifest}); err != nil {
				t.Fatalf("EnsurePrefix: %v", err)
			}
			if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: suffix, Manifest: manifest}); err != nil {
				t.Fatalf("PrefillSuffix: %v", err)
			}
		},
		Decode: func(ctx context.Context, sess transport.Session) (string, error) {
			temp := 0.0
			seed := 7
			return sessiontest.DecodeText(ctx, sess, transport.DecodeConfig{MaxTokens: decodeN, Temperature: &temp, Seed: &seed})
		},
		Width:           width,
		EmptyDecodeSkip: "tiny model produced no visible token for the reference continuation",
	})
}
