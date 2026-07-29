package openvino

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
	"github.com/contenox/contenox/internal/transport"
)

// fakeVLMBackend is a no-CGO vlmBackend for adapter-logic unit tests.
type fakeVLMBackend struct {
	lastPrompt string
	lastImages []ovsession.VLMImage
}

func (f *fakeVLMBackend) ApplyChatTemplate(messages []ovsession.ChatMessage, addGenerationPrompt bool) (string, error) {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	if addGenerationPrompt {
		b.WriteString("assistant:")
	}
	return b.String(), nil
}

func (f *fakeVLMBackend) Tokenize(_ context.Context, prompt string, _ bool) ([]int, error) {
	toks := make([]int, 0, len(prompt)/2)
	for i, field := range strings.Fields(prompt) {
		_ = field
		toks = append(toks, i)
	}
	return toks, nil
}

func (f *fakeVLMBackend) Stream(_ context.Context, prompt string, images []ovsession.VLMImage, _ ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error) {
	f.lastPrompt = prompt
	f.lastImages = images
	ch := make(chan ovsession.StreamChunk, 1)
	ch <- ovsession.StreamChunk{Text: "ok"}
	close(ch)
	return ch, nil
}

func (f *fakeVLMBackend) Close() error { return nil }

func TestUnit_OpenVINOVisionMarkersTranslate(t *testing.T) {
	msgs := []ovsession.ChatMessage{
		{Role: "user", Content: transport.MediaMarker + " first"},
		{Role: "assistant", Content: "a color"},
		{Role: "user", Content: "second " + transport.MediaMarker + " and " + transport.MediaMarker},
	}
	out, err := translateMediaMarkers(msgs, 3)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out[0].Content != "<ov_genai_image_0> first" {
		t.Fatalf("first message: %q", out[0].Content)
	}
	if out[2].Content != "second <ov_genai_image_1> and <ov_genai_image_2>" {
		t.Fatalf("third message: %q", out[2].Content)
	}
	// Input must not be mutated.
	if !strings.Contains(msgs[0].Content, transport.MediaMarker) {
		t.Fatal("translateMediaMarkers mutated its input")
	}

	if _, err := translateMediaMarkers(msgs, 2); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("marker/image mismatch should be ErrUnsupportedFeature, got %v", err)
	}
}

func TestUnit_OpenVINOVisionModelDirDetection(t *testing.T) {
	dir := t.TempDir()
	if isVLMModelDir(dir) {
		t.Fatal("empty dir must not be a VLM dir")
	}
	if err := os.WriteFile(filepath.Join(dir, openvinoLanguageModelXML), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isVLMModelDir(dir) {
		t.Fatal("language model alone must not be a VLM dir")
	}
	if err := os.WriteFile(filepath.Join(dir, openvinoVisionEmbeddingsXML), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isVLMModelDir(dir) {
		t.Fatal("language + vision embeddings IRs must be detected as a VLM dir")
	}
}

func TestUnit_OpenVINOVisionTokensPerImageEstimate(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   int
	}{
		{"gemma3-declared", `{"mm_tokens_per_image": 256}`, 256},
		{"gemma4-declared", `{"vision_soft_tokens_per_image": 262}`, 262},
		{"internvl-geometry", `{"force_image_size": 448, "downsample_ratio": 0.5, "vision_config": {"patch_size": 14}}`, 256},
		{"vision-config-image-size", `{"downsample_ratio": 0.5, "vision_config": {"image_size": 448, "patch_size": 14}}`, 256},
		{"insufficient", `{"model_type": "whatever"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := visionTokensPerImageEstimate(dir); got != tc.want {
				t.Fatalf("estimate = %d, want %d", got, tc.want)
			}
		})
	}
	if got := visionTokensPerImageEstimate(t.TempDir()); got != 0 {
		t.Fatalf("missing config.json should estimate 0, got %d", got)
	}
}

func TestUnit_OpenVINOVisionTextSessionRejectsImages(t *testing.T) {
	ctx := context.Background()
	img := []transport.ImagePart{{Data: []byte{1, 2, 3}, MimeType: "image/png"}}

	sess := newGenaiSession(nil, 4096)
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "stable", Images: img}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("EnsurePrefix with images on token-only session: want ErrUnsupportedFeature, got %v", err)
	}
	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: "turn", Images: img}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("PrefillSuffix with images on token-only session: want ErrUnsupportedFeature, got %v", err)
	}
}

func TestUnit_OpenVINOVisionSessionContracts(t *testing.T) {
	ctx := context.Background()
	backend := &fakeVLMBackend{}
	sess := newVisionSession(backend, 4096, 256)

	// Stable prefixes cannot carry images or tool definitions.
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{
		Text:   "stable",
		Images: []transport.ImagePart{{Data: []byte{1}}},
	}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("prefix images: want ErrUnsupportedFeature, got %v", err)
	}
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{
		Text:  "stable",
		Tools: `[{"type":"function"}]`,
	}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("prefix tools: want ErrUnsupportedFeature, got %v", err)
	}

	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "You are terse."}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}

	// Marker/image count mismatch is refused before any probe or backend work.
	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{
		Text:   "look: " + transport.MediaMarker,
		Images: nil,
	}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("marker without image: want ErrUnsupportedFeature, got %v", err)
	}

	// A decodable-image suffix accounts text + vision tokens.
	restore := probeVLMImage
	probeVLMImage = func([]byte) error { return nil }
	defer func() { probeVLMImage = restore }()
	st, err := sess.PrefillSuffix(ctx, transport.SuffixInput{
		Text:   "describe " + transport.MediaMarker + " briefly",
		Images: []transport.ImagePart{{Data: []byte{1, 2, 3}, MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	if st.ResidentTokens <= 256 {
		t.Fatalf("resident estimate should include the per-image vision tokens, got %d", st.ResidentTokens)
	}

	// Decode routes the translated prompt and the images to the backend.
	ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: 8})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for range ch {
	}
	if !strings.Contains(backend.lastPrompt, "<ov_genai_image_0>") {
		t.Fatalf("decode prompt should carry the universal image tag, got %q", backend.lastPrompt)
	}
	if strings.Contains(backend.lastPrompt, transport.MediaMarker) {
		t.Fatalf("decode prompt must not leak the media marker, got %q", backend.lastPrompt)
	}
	if len(backend.lastImages) != 1 {
		t.Fatalf("decode should pass 1 image, got %d", len(backend.lastImages))
	}

	// Structured output and parser protocols are refused in v1.
	if _, err := sess.Decode(ctx, transport.DecodeConfig{
		StructuredOutput: transport.StructuredOutputConfig{Protocol: "openvino:json_schema_tool_calls"},
	}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("structured output: want ErrUnsupportedFeature, got %v", err)
	}
	if _, err := sess.Decode(ctx, transport.DecodeConfig{
		ParserProtocols: []string{"openvino:reasoning_parser"},
	}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("parser protocols: want ErrUnsupportedFeature, got %v", err)
	}

	// Snapshot/restore are refused (images cannot ride a snapshot).
	if _, err := sess.Snapshot(ctx); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("snapshot: want ErrUnsupportedFeature, got %v", err)
	}
	if err := sess.Restore(ctx, transport.SessionSnapshot{}); !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("restore: want ErrUnsupportedFeature, got %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "x"}); !errors.Is(err, transport.ErrSessionClosed) {
		t.Fatalf("EnsurePrefix after close: want ErrSessionClosed, got %v", err)
	}
}
