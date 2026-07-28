package modelrepo

import "strings"

// The Gemini and Vertex model-listing APIs report no input-modality field,
// unlike Ollama, Bedrock, and Anthropic, where vision support is detected at
// runtime. Google model vision support is therefore maintained here by hand
// from https://ai.google.dev/gemini-api/docs/models: the mainline
// generateContent families are multimodal, while the same family names also
// cover embedding/TTS/speech-translation/generation models excluded by
// geminiNonVisionMarkers.
//
// Add new multimodal family prefixes to geminiVisionFamilies, and new
// non-vision variants within an existing family to geminiNonVisionMarkers.
var geminiVisionFamilies = []string{
	"gemini-1.5",
	"gemini-2.0",
	"gemini-2.5",
	"gemini-3", // covers gemini-3, -3.1, -3.5, -3.6, ...
	"gemini-pro-vision",
}

// geminiNonVisionMarkers are substrings that mean a model takes no image input
// even when its family prefix is otherwise multimodal (embedding, TTS, speech
// translation, and music/video/image generation models).
var geminiNonVisionMarkers = []string{
	"embedding",
	"tts",
	"live-translate",
	"lyria",
	"veo",
	"imagen",
	"aqa",
}

// GeminiModelSupportsVision reports whether a Google (Gemini/Vertex) model accepts
// image input, from the hand-maintained allowlist above. It is a fallback used
// because the Gemini and Vertex model-listing APIs expose no input-modality field;
// every other provider detects this from its API. A model name may be bare
// ("gemini-2.5-pro") or API-qualified ("models/gemini-2.5-pro",
// "publishers/google/models/gemini-2.5-pro") — the trailing segment is matched.
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
