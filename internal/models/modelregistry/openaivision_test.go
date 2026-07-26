package modelregistry_test

import (
	"testing"

	"github.com/contenox/beam/internal/models/modelregistry"
)

func TestUnit_OpenAIModelSupportsVision(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Vision-capable families.
		{"gpt-4o", true},
		{"gpt-4o-2024-08-06", true},
		{"gpt-4o-mini", true},
		{"gpt-4.1", true},
		{"gpt-4.1-mini", true},
		{"gpt-4-turbo", true},
		{"gpt-4-turbo-2024-04-09", true},
		{"chatgpt-4o-latest", true},
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5.6-sol", true},
		{"gpt-5.4-nano", true},
		{"o1", true},
		{"o1-2024-12-17", true},
		{"o1-pro", true},
		{"o3", true},
		{"o3-pro", true},
		{"o4-mini", true},
		{"computer-use-preview", true},
		{"gpt-4-vision-preview", true},
		{"gpt-4-1106-vision-preview", true},

		// Text-only chat/reasoning — the landmines.
		{"gpt-4", false},               // base gpt-4 is text-only
		{"gpt-4-0613", false},          // ...
		{"gpt-4-32k", false},           // ...
		{"gpt-4-turbo-preview", false}, // deprecated text-only alias
		{"gpt-3.5-turbo", false},
		{"o1-mini", false},
		{"o1-preview", false},
		{"o3-mini", false},

		// Non-chat / non-vision modalities.
		{"gpt-4o-audio-preview", false},
		{"gpt-4o-realtime-preview", false},
		{"gpt-4o-transcribe", false},
		{"gpt-4o-mini-transcribe", false},
		{"gpt-4o-search-preview", false},
		{"text-embedding-3-large", false},
		{"tts-1", false},
		{"whisper-1", false},
		{"dall-e-3", false},
		{"gpt-image-1", false},
		{"omni-moderation-latest", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := modelregistry.OpenAIModelSupportsVision(tc.name); got != tc.want {
			t.Errorf("OpenAIModelSupportsVision(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
