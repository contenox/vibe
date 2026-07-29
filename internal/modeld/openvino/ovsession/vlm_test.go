//go:build openvino && openvino_genai

package ovsession

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// solidPNG encodes a solid-color PNG in memory for decoder tests.
func solidPNG(t *testing.T, c color.RGBA, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestUnit_OpenVINOVisionCell_FailurePaths exercises the vlm cell's failure
// surface without any model: session construction on a bogus directory and
// image probing on undecodable bytes must fail with descriptive errors.
func TestUnit_OpenVINOVisionCell_FailurePaths(t *testing.T) {
	if _, err := NewVLM("", "CPU"); err == nil {
		t.Fatal("NewVLM with empty dir should fail")
	}
	if _, err := NewVLM("/nonexistent/openvino/vlm/dir", "CPU"); err == nil {
		t.Fatal("NewVLM with bogus dir should fail")
	} else if err.Error() == "" {
		t.Fatal("bogus dir error should carry a message")
	}

	if _, _, err := ProbeVLMImage(nil); err == nil {
		t.Fatal("probe of empty bytes should fail")
	}
	if _, _, err := ProbeVLMImage([]byte("definitely not an image")); err == nil {
		t.Fatal("probe of bogus bytes should fail")
	} else if !strings.Contains(err.Error(), "image") {
		t.Fatalf("bogus image error should mention image decode, got %v", err)
	}

	w, h, err := ProbeVLMImage(solidPNG(t, color.RGBA{R: 255, A: 255}, 32, 24))
	if err != nil {
		t.Fatalf("probe of valid png: %v", err)
	}
	if w != 32 || h != 24 {
		t.Fatalf("probe dimensions = %dx%d, want 32x24", w, h)
	}
}
