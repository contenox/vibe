package llmresolver

import (
	"github.com/contenox/contenox/libtracker"
)

type Request struct {
	ProviderTypes []string
	ModelNames    []string
	ContextLength int
	// RequiresVision restricts chat/stream resolution to vision-capable
	// providers. Callers derive it from the presence of image attachments in
	// the request's messages (see modelrepo.MessagesHaveImages) rather than
	// setting it by hand.
	RequiresVision bool
	Tracker        libtracker.ActivityTracker
}

type EmbedRequest struct {
	ModelName    string
	ProviderType string
	Tracker      libtracker.ActivityTracker
}
