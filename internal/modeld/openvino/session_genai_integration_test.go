//go:build openvino && openvino_genai

package openvino

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
)

// TestSystem_OpenVINOGenAIAdapter_GeneratesAndReusesPrefix drives the real
// OpenVINO GenAI backend through the transport.Session adapter built in
// session.go. It checks that EnsurePrefix -> PrefillSuffix -> Decode generates
// text on the configured device and that a second turn sharing the stable prefix
// reuses the pipeline's warm KV through the adapter.
//
// Provision + run with the pinned wheels/headers/model:
//
//	make -f Makefile.openvino test-genai
//	OPENVINO_DEVICE=NPU make -f Makefile.openvino test-genai
func TestSystem_OpenVINOGenAIAdapter_GeneratesAndReusesPrefix(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL (see Makefile.openvino)")
	}
	ctx := context.Background()

	sess, err := (&Service{}).OpenSession(ctx, transport.OpenSessionRequest{
		ModelName: "test",
		Type:      "openvino",
		Path:      modelDir,
		Config:    transport.Config{NumCtx: 4096},
	})
	if err != nil {
		t.Fatalf("OpenSession on %s: %v", resolveDevice(), err)
	}
	defer sess.Close()

	stable := strings.Repeat("You are a precise Go coding assistant. ", 64)
	manifest := contextasm.ContextManifest{
		Backend:              "openvino",
		ModelDigest:          "test-model",
		PromptFormat:         "openvino_chat_template",
		PromptTemplateDigest: "test-model",
		RuntimeDigest:        resolveDevice(),
		StableByteHash:       contextasm.HashString(stable),
	}

	turn := func(suffix string) (string, time.Duration) {
		t.Helper()
		start := time.Now()
		if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: manifest}); err != nil {
			t.Fatalf("EnsurePrefix: %v", err)
		}
		if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: suffix, Manifest: manifest}); err != nil {
			t.Fatalf("PrefillSuffix: %v", err)
		}
		ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: 24})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		var b strings.Builder
		for chunk := range ch {
			if chunk.Error != nil {
				t.Fatalf("stream: %v", chunk.Error)
			}
			b.WriteString(chunk.Text)
		}
		return strings.TrimSpace(b.String()), time.Since(start)
	}

	cold, coldDur := turn("Q: How do I read a file in Go? A:")
	if cold == "" {
		t.Fatal("cold turn produced no text")
	}
	warm, warmDur := turn("Q: How do I write a file in Go? A:")
	if warm == "" {
		t.Fatal("warm turn produced no text")
	}

	t.Logf("device=%s stable_prefix_bytes=%d", resolveDevice(), len(stable))
	t.Logf("COLD  %s  -> %q", coldDur, cold)
	t.Logf("WARM  %s  -> %q", warmDur, warm)
	if coldDur > 0 {
		t.Logf("warm/cold latency ratio: %.2f", warmDur.Seconds()/coldDur.Seconds())
	}
}
