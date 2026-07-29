//go:build openvino && openvino_genai

package openvino

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

func solidRedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func visionManifest(stable, suffix string) contextasm.ContextManifest {
	return contextasm.ContextManifest{
		Backend:              "openvino",
		ModelDigest:          "vision-test-model",
		PromptFormat:         "openvino_chat_template",
		PromptTemplateDigest: "vision-test-model",
		RuntimeDigest:        resolveDevice(),
		StableByteHash:       contextasm.HashString(stable),
		StableBytes:          len(stable),
		TotalBytes:           len(stable) + len(suffix),
		Segments: []contextasm.ManifestSegment{
			{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: contextasm.HashString(stable)},
			{Kind: "user", Stable: false, ByteStart: len(stable), ByteEnd: len(stable) + len(suffix), ByteHash: contextasm.HashString(suffix)},
		},
	}
}

// TestSystem_OpenVINOVisionSession_DescribeAndDecode proves the transport flow
// end to end on a real exported OpenVINO VLM: Describe certifies vision from
// the IR layout, OpenSession routes to the vision session, and a volatile
// suffix carrying a MediaMarker plus a solid-red PNG streams back an answer
// naming the color.
//
//	make -f Makefile.openvino test-genai-vision
func TestSystem_OpenVINOVisionSession_DescribeAndDecode(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_VISION_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_VISION_MODEL (see Makefile.openvino test-genai-vision)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	svc := NewService()
	req := transport.OpenSessionRequest{
		ModelName: "vision-test",
		Type:      "openvino",
		Path:      modelDir,
		Config:    transport.Config{NumCtx: 4096},
	}

	info, err := svc.Describe(ctx, req)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !info.SupportsVision {
		t.Fatal("Describe on a VLM dir must set SupportsVision")
	}
	if info.VisionTokensPerImage <= 0 {
		t.Fatalf("Describe should estimate vision tokens per image, got %d", info.VisionTokensPerImage)
	}
	t.Logf("Describe: vision_tokens_per_image=%d effective_context=%d weights=%d", info.VisionTokensPerImage, info.EffectiveContext, info.WeightsBytes)

	sess, err := svc.OpenSession(ctx, req)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()
	if _, ok := sess.(*visionSession); !ok {
		t.Fatalf("OpenSession on a VLM dir should return the vision session, got %T", sess)
	}

	stable := "You are a terse vision assistant."
	suffix := transport.MediaMarker + "What color is this image? Answer with one word."
	mani := visionManifest(stable, suffix)

	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: mani}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	st, err := sess.PrefillSuffix(ctx, transport.SuffixInput{
		Text:     suffix,
		Manifest: mani,
		Images:   []transport.ImagePart{{Data: solidRedPNG(t, 224, 224), MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	if st.ResidentTokens <= info.VisionTokensPerImage {
		t.Fatalf("suffix status should account image tokens: resident=%d", st.ResidentTokens)
	}

	temp := 0.0
	start := time.Now()
	ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: 12, Temperature: &temp})
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
	out := strings.TrimSpace(b.String())
	t.Logf("vision answer in %s: %q", time.Since(start), out)
	if !strings.Contains(strings.ToLower(out), "red") {
		t.Fatalf("expected the answer to name the color red, got %q", out)
	}
}

// TestSystem_OpenVINOVisionRejection_TextOnlyModel proves refuse-don't-spill
// at the session boundary: image input against a text-only OpenVINO model is
// rejected with the typed unsupported-feature error instead of being silently
// dropped from the prompt.
func TestSystem_OpenVINOVisionRejection_TextOnlyModel(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_TEST_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL (see Makefile.openvino)")
	}
	if isVLMModelDir(modelDir) {
		t.Fatalf("CONTENOX_OPENVINO_TEST_MODEL should be a text-only model, %s looks like a VLM dir", modelDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sess, err := NewService().OpenSession(ctx, transport.OpenSessionRequest{
		ModelName: "text-test",
		Type:      "openvino",
		Path:      modelDir,
		Config:    transport.Config{NumCtx: 2048},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()

	stable := "You are terse."
	suffix := transport.MediaMarker + "What color is this image?"
	mani := visionManifest(stable, suffix)
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: mani}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	_, err = sess.PrefillSuffix(ctx, transport.SuffixInput{
		Text:     suffix,
		Manifest: mani,
		Images:   []transport.ImagePart{{Data: solidRedPNG(t, 32, 32), MimeType: "image/png"}},
	})
	if !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("image input to a text-only model: want ErrUnsupportedFeature, got %v", err)
	}
}
