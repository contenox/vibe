//go:build openvino && openvino_genai

package ovsession

import (
	"context"
	"image/color"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSystem_OpenVINOVisionCell_AnswersImageColor drives the real VLMPipeline
// on the configured device: a synthetic solid-red PNG plus a one-word color
// question must stream back an answer containing "red". The prompt is
// self-templated with the model's own chat template and carries the universal
// <ov_genai_image_0> tag the pipeline normalizes into native vision tags.
//
// Provision + run with the pinned wheels/headers/models:
//
//	make -f Makefile.openvino test-genai-vision
func TestSystem_OpenVINOVisionCell_AnswersImageColor(t *testing.T) {
	modelDir := os.Getenv("CONTENOX_OPENVINO_VISION_MODEL")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_VISION_MODEL (see Makefile.openvino test-genai-vision)")
	}
	device := os.Getenv("CONTENOX_OPENVINO_TEST_DEVICE")

	start := time.Now()
	sess, err := NewVLM(modelDir, device)
	if err != nil {
		t.Fatalf("NewVLM on %s: %v", device, err)
	}
	defer sess.Close()
	t.Logf("VLM pipeline loaded in %s", time.Since(start))

	prompt, err := sess.ApplyChatTemplate([]ChatMessage{
		{Role: "user", Content: "<ov_genai_image_0>What color is this image? Answer with one word."},
	}, true)
	if err != nil {
		t.Fatalf("ApplyChatTemplate: %v", err)
	}
	if strings.Contains(prompt, "ApplyChatTemplate") || !strings.Contains(prompt, "What color") {
		t.Fatalf("templated prompt looks wrong: %q", prompt)
	}

	red := solidPNG(t, color.RGBA{R: 255, A: 255}, 224, 224)
	temp := 0.0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	genStart := time.Now()
	out, err := sess.Generate(ctx, prompt, []VLMImage{{Data: red, MimeType: "image/png"}}, GenerateOptions{
		MaxNewTokens: 12,
		Temperature:  &temp,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("VLM answer in %s: %q", time.Since(genStart), out)
	if !strings.Contains(strings.ToLower(out), "red") {
		t.Fatalf("expected the answer to name the color red, got %q", out)
	}
}
