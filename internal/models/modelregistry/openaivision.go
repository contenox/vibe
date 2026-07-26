package modelregistry

import "strings"

// OpenAI's /v1/models is a bare list (id/created/owned_by) with no modality
// field, and OpenAI is a closed first-party set, so vision support is maintained
// here from the published model docs (verified 2026-07-22 against the gpt-5.x —
// through 5.6 — and o-series lineups).
// Source: https://developers.openai.com/api/docs/guides/images-vision
//
// The family is full of naming landmines this must respect:
//   - base gpt-4 (gpt-4, gpt-4-0613, gpt-4-32k) is TEXT-ONLY, while
//     gpt-4-turbo / gpt-4o / gpt-4.1 have vision;
//   - the gpt-4o prefix also spans audio/transcribe/realtime variants that take
//     no chat image;
//   - the reasoning minis split: o1 / o3 / o4-mini have vision, but o1-mini,
//     o1-preview, and o3-mini are text-only.
//
// Precedence: non-vision markers → legacy vision-preview snapshots → text-only
// chat/reasoning prefixes → vision family prefixes → default false. The
// capability override wins over all of it (runtimestate/catalogstate.go), so any
// miss on a deprecated snapshot is correctable by declaration.
//
// MAINTENANCE: add a family prefix to openAIVisionPrefixes when OpenAI ships a
// new multimodal family; add a prefix to openAITextOnlyPrefixes for a text-only
// variant that a vision prefix would otherwise capture.
var (
	openAINonVisionMarkers = []string{
		"embedding", "tts", "whisper", "transcribe", "-audio", "realtime",
		"dall-e", "sora", "gpt-image", "moderation", "-search",
	}
	openAITextOnlyPrefixes = []string{
		"gpt-3.5", "gpt-4-turbo-preview", "o1-mini", "o1-preview", "o3-mini",
	}
	openAIVisionPrefixes = []string{
		"gpt-5", "gpt-4o", "gpt-4.1", "gpt-4-turbo", "chatgpt-4o",
		"o1", "o3", "o4", "computer-use",
	}
)

// OpenAIModelSupportsVision reports whether an OpenAI model id accepts image
// input, from the maintained list above. It exists because OpenAI's model API
// reports no modality; the capability override always takes precedence.
func OpenAIModelSupportsVision(modelName string) bool {
	n := strings.ToLower(strings.TrimSpace(modelName))
	for _, m := range openAINonVisionMarkers {
		if strings.Contains(n, m) {
			return false
		}
	}
	// Legacy dated vision snapshots (gpt-4-vision-preview, gpt-4-1106-vision-preview).
	if strings.Contains(n, "vision-preview") {
		return true
	}
	for _, p := range openAITextOnlyPrefixes {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	for _, p := range openAIVisionPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
