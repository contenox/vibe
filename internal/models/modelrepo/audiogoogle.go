package modelrepo

import "strings"

var geminiAudioFamilies = []string{
	"gemini-1.5",
	"gemini-2.0",
	"gemini-2.5",
	"gemini-3", // covers gemini-3, -3.1, -3.5, -3.6, ...
}

var geminiNonAudioMarkers = []string{
	"embedding",
	"tts",
	"live-translate",
	"lyria",
	"veo",
	"imagen",
	"aqa",
}

// GeminiModelSupportsAudio reports whether a Google model accepts audio input, matched against the hand-maintained allowlist by its trailing name segment.
func GeminiModelSupportsAudio(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, marker := range geminiNonAudioMarkers {
		if strings.Contains(name, marker) {
			return false
		}
	}
	for _, family := range geminiAudioFamilies {
		if strings.HasPrefix(name, family) {
			return true
		}
	}
	return false
}
