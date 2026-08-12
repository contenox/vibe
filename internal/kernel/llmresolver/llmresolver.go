package llmresolver

import (
	"github.com/contenox/contenox/libtracker"
)

type Request struct {
	ProviderTypes []string
	ModelNames    []string
	ContextLength int
	// RequiresVision restricts chat/stream resolution to vision-capable providers.
	RequiresVision bool
	// RequiresAudio restricts chat/stream resolution to audio-capable providers.
	RequiresAudio bool
	Tracker       libtracker.ActivityTracker
}

type EmbedRequest struct {
	ModelName    string
	ProviderType string
	Tracker      libtracker.ActivityTracker
}
