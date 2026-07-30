//go:build llamanode && llamacpp_direct

package llamasession

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

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
	"github.com/contenox/contenox/libtransport"
)

func TestUnit_VisionImageCellSentinelStableAndNegative(t *testing.T) {
	a1, a2 := imageCellSentinel("11414370945977883648"), imageCellSentinel("11414370945977883648")
	b := imageCellSentinel("5577006791947779410")
	if a1 != a2 {
		t.Fatalf("sentinel not deterministic: %d vs %d", a1, a2)
	}
	if a1 == b {
		t.Fatalf("distinct image IDs mapped to one sentinel %d", a1)
	}
	for _, v := range []int{a1, b, imageCellSentinel("")} {
		if v >= -1 {
			t.Fatalf("sentinel %d must be < -1 (never a token ID, never llama's null token)", v)
		}
	}
}

// requireVLM resolves the vision test model pair (see Makefile.llamacpp-direct
// test-session-vision, which hard-errors when the files are absent so these
// skips can never mask a false green in CI).
func requireVLM(t *testing.T) (modelPath, mmprojPath string) {
	t.Helper()
	modelPath = os.Getenv("CONTENOX_LLAMA_VLM_GGUF")
	mmprojPath = os.Getenv("CONTENOX_LLAMA_VLM_MMPROJ")
	if modelPath == "" || mmprojPath == "" {
		t.Skip("set CONTENOX_LLAMA_VLM_GGUF and CONTENOX_LLAMA_VLM_MMPROJ to run vision system tests")
	}
	// The session derives the projector from the model path (the two-file
	// store convention); a mismatched env layout would silently test nothing.
	if got := llama.MMProjPathFor(modelPath); got != mmprojPath {
		t.Fatalf("MMProjPathFor(%q) = %q, want the configured projector %q (keep mmproj.gguf next to model.gguf)", modelPath, got, mmprojPath)
	}
	return modelPath, mmprojPath
}

// solidPNG encodes a uniform-color image, the least ambiguous probe a VLM can
// be asked about.
func solidPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 224, 224))
	for y := 0; y < 224; y++ {
		for x := 0; x < 224; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func visionManifest(stable string, volatile []chatTemplateMessage) llama.ContextManifest {
	var volatileText strings.Builder
	segs := []llama.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: shaHex(stable)},
	}
	off := len(stable)
	for _, m := range volatile {
		segs = append(segs, llama.ManifestSegment{
			Kind: m.Role, Stable: false,
			ByteStart: off, ByteEnd: off + len(m.Content), ByteHash: shaHex(m.Content),
		})
		off += len(m.Content)
		volatileText.WriteString(m.Content)
	}
	return llama.ContextManifest{
		ProfileID:            "vision-test",
		Backend:              "llamacpp",
		BackendVersion:       "test",
		ModelDigest:          "vlm",
		PromptFormat:         "chatml",
		PromptTemplateDigest: "test-template",
		RuntimeDigest:        "test-runtime",
		AddBOS:               false,
		StableBytes:          len(stable),
		TotalBytes:           off,
		StableByteHash:       shaHex(stable),
		Segments:             segs,
	}
}

func decodeAll(ctx context.Context, t *testing.T, sess llama.Session, maxTokens int) string {
	t.Helper()
	temp := 0.0
	seed := 7
	chunks, err := sess.Decode(ctx, llama.DecodeConfig{MaxTokens: maxTokens, Temperature: &temp, Seed: &seed})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			t.Fatalf("decode error: %v", chunk.Error)
		}
		out.WriteString(chunk.Text)
	}
	return out.String()
}

// The full local vision loop on real hardware: a solid red PNG goes in as a
// suffix image part, the projector encodes it, and the model must actually see
// it — the reply names the color. A second turn resends the image with the
// conversation history (the multi-turn contract) and must keep working over
// the reused stable prefix.
func TestSystem_LlamaSessionVision_RedImageOneWordAnswer(t *testing.T) {
	modelPath, _ := requireVLM(t)
	if llamacppshim.MediaMarker() != transport.MediaMarker {
		t.Fatalf("transport.MediaMarker %q drifted from mtmd marker %q", transport.MediaMarker, llamacppshim.MediaMarker())
	}

	sess, err := New(modelPath, llama.Config{
		NumCtx: 4096, NumBatch: 512, NumThreads: 4, NumGpuLayers: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	stable := "You are a concise assistant."
	red := solidPNG(t, color.RGBA{R: 255, A: 255})
	turn1 := chatTemplateMessage{Role: "user", Content: transport.MediaMarker + "\nWhat color is this image? Answer with one word."}

	m := visionManifest(stable, []chatTemplateMessage{turn1})
	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatal(err)
	}
	status, err := sess.PrefillSuffix(ctx, llama.SuffixInput{
		Text:     turn1.Content,
		Manifest: m,
		Images:   []transport.ImagePart{{Data: red, MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.SuffixTokens == 0 || status.ResidentTokens <= status.PrefixTokens {
		t.Fatalf("suffix status missing image-bearing tokens: %+v", status)
	}
	answer1 := decodeAll(ctx, t, sess, 8)
	t.Logf("turn 1 answer: %q", answer1)
	if !strings.Contains(strings.ToLower(answer1), "red") {
		t.Fatalf("model did not see the red image: answer = %q", answer1)
	}

	// Turn 2: the history (image marker included) plus a new question, image
	// bytes resent. The stable prefix must reuse warm.
	turn2 := []chatTemplateMessage{
		turn1,
		{Role: "assistant", Content: strings.TrimSpace(answer1)},
		{Role: "user", Content: "Now answer with one word again: is the image bright or dark?"},
	}
	m2 := visionManifest(stable, turn2)
	var volatileText strings.Builder
	for _, msg := range turn2 {
		volatileText.WriteString(msg.Content)
	}
	prefix2, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m2})
	if err != nil {
		t.Fatal(err)
	}
	if prefix2.ReusedTokens != prefix2.PrefixTokens || prefix2.PrefixTokens == 0 {
		t.Fatalf("turn 2 did not reuse the stable prefix: %+v", prefix2)
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{
		Text:     volatileText.String(),
		Manifest: m2,
		Images:   []transport.ImagePart{{Data: red, MimeType: "image/png"}},
	}); err != nil {
		t.Fatal(err)
	}
	answer2 := decodeAll(ctx, t, sess, 8)
	t.Logf("turn 2 answer: %q", answer2)
	if strings.TrimSpace(answer2) == "" {
		t.Fatal("turn 2 produced no answer")
	}
}

// The typed refusals around image input: images never enter the stable prefix,
// marker/image counts must agree, and a marker left in a text-only turn is a
// contract breach — each refused loudly, none crashing the session.
func TestSystem_LlamaSessionVision_TypedRefusals(t *testing.T) {
	modelPath, _ := requireVLM(t)

	sess, err := New(modelPath, llama.Config{
		NumCtx: 2048, NumBatch: 512, NumThreads: 4, NumGpuLayers: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	stable := "You are a concise assistant."
	red := solidPNG(t, color.RGBA{R: 255, A: 255})
	user := chatTemplateMessage{Role: "user", Content: transport.MediaMarker + "\nWhat is this?"}
	m := visionManifest(stable, []chatTemplateMessage{user})

	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{
		Text:     stable,
		Manifest: m,
		Images:   []transport.ImagePart{{Data: red, MimeType: "image/png"}},
	}); !errors.Is(err, llama.ErrUnsupportedFeature) {
		t.Fatalf("EnsurePrefix with images = %v, want ErrUnsupportedFeature", err)
	}

	if _, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatal(err)
	}
	// Marker in the prompt, no image parts: the text path must refuse instead
	// of feeding the model a literal marker for an image it never received.
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: user.Content, Manifest: m}); err == nil ||
		!strings.Contains(err.Error(), "media marker") {
		t.Fatalf("marker without images = %v, want media marker contract error", err)
	}
	// Image parts without a marker: mtmd's count mismatch surfaces as a clear
	// error, not a crash.
	noMarker := chatTemplateMessage{Role: "user", Content: "What is this?"}
	mNoMarker := visionManifest(stable, []chatTemplateMessage{noMarker})
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{
		Text:     noMarker.Content,
		Manifest: mNoMarker,
		Images:   []transport.ImagePart{{Data: red, MimeType: "image/png"}},
	}); err == nil || !strings.Contains(err.Error(), "media marker") {
		t.Fatalf("images without marker = %v, want media marker count error", err)
	}
	// Garbage bytes must fail image decoding cleanly.
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{
		Text:     user.Content,
		Manifest: m,
		Images:   []transport.ImagePart{{Data: []byte("not an image"), MimeType: "image/png"}},
	}); err == nil {
		t.Fatal("corrupt image bytes accepted, want decode error")
	}

	// The session survives every refusal: a real turn still works.
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{
		Text:     user.Content,
		Manifest: m,
		Images:   []transport.ImagePart{{Data: red, MimeType: "image/png"}},
	}); err != nil {
		t.Fatalf("session unusable after refusals: %v", err)
	}
}

// Describe on the real model pair must certify vision from the projector
// metadata and produce a usable per-image token estimate.
func TestSystem_LlamaSessionVision_DescribeCertifiesVision(t *testing.T) {
	modelPath, _ := requireVLM(t)

	svc := llama.NewService()
	info, err := svc.Describe(context.Background(), transport.OpenSessionRequest{
		Type: "llama",
		Path: modelPath,
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !info.SupportsVision {
		t.Fatalf("Describe did not certify vision for a real VLM: %+v", info)
	}
	if info.VisionTokensPerImage <= 0 {
		t.Fatalf("VisionTokensPerImage = %d, want a positive planning estimate", info.VisionTokensPerImage)
	}
	t.Logf("SupportsVision=%v VisionTokensPerImage=%d OverheadBytes=%d", info.SupportsVision, info.VisionTokensPerImage, info.OverheadBytes)
}
