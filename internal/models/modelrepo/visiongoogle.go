package modelrepo

import "strings"

// The Gemini and Vertex listing APIs report no input-modality field, so vision support is
// maintained by hand from https://ai.google.dev/gemini-api/docs/models: add multimodal family
// prefixes here, and non-vision variants within a family to geminiNonVisionMarkers.
var geminiVisionFamilies = []string{
	"gemini-1.5",
	"gemini-2.0",
	"gemini-2.5",
	"gemini-3", // covers gemini-3, -3.1, -3.5, -3.6, ...
	"gemini-pro-vision",
}

var geminiNonVisionMarkers = []string{
	"embedding",
	"tts",
	"live-translate",
	"lyria",
	"veo",
	"imagen",
	"aqa",
}

// GeminiModelSupportsVision reports whether a Google model accepts image input,
// from the hand-maintained allowlist above. The name may be bare or
// API-qualified; the trailing segment is matched.
func GeminiModelSupportsVision(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, marker := range geminiNonVisionMarkers {
		if strings.Contains(name, marker) {
			return false
		}
	}
	for _, family := range geminiVisionFamilies {
		if strings.HasPrefix(name, family) {
			return true
		}
	}
	return false
}
