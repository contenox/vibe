package modelregistry_test

import (
	"testing"

	"github.com/contenox/beam/internal/models/modelregistry"
)

func TestUnit_GeminiModelSupportsVision(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Multimodal chat families — image input.
		{"gemini-2.5-pro", true},
		{"gemini-2.5-flash-lite", true},
		{"gemini-3.1-pro-preview", true},
		{"gemini-1.5-pro", true},
		{"gemini-pro-vision", true},
		// API-qualified names must match on the trailing segment.
		{"models/gemini-2.5-pro", true},
		{"publishers/google/models/gemini-3-flash-preview", true},
		// Same families, but non-vision variants excluded by markers.
		{"gemini-2.5-flash-preview-tts", false},
		{"gemini-embedding-001", false},
		{"gemini-2.5-flash-live-translate-preview", false},
		{"lyria-3-pro-preview", false},
		{"veo-3.1-generate-preview", false},
		{"imagen-4.0-generate", false},
		// Unknown / non-Gemini — conservative default is no vision.
		{"text-embedding-004", false},
		{"some-random-model", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := modelregistry.GeminiModelSupportsVision(tc.name); got != tc.want {
			t.Errorf("GeminiModelSupportsVision(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
