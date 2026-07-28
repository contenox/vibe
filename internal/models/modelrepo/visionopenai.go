package modelrepo

import "strings"

// OpenAI's /v1/models is a bare list (id/created/owned_by) with no modality
// field, so vision support is maintained here from
// https://developers.openai.com/api/docs/guides/images-vision. Naming
// landmines to respect: base gpt-4 is text-only while gpt-4-turbo/gpt-4o/
// gpt-4.1 have vision; the gpt-4o prefix also spans audio/transcribe/realtime
// variants with no chat image; the reasoning minis split (o1/o3/o4-mini have
// vision, o1-mini/o1-preview/o3-mini don't).
//
// Precedence: non-vision markers → legacy vision-preview snapshots → text-only
// prefixes → vision prefixes → default false. The capability override always
// wins (runtimestate/catalogstate.go), so a miss here is correctable by
// declaration. Add new family prefixes to openAIVisionPrefixes /
// openAITextOnlyPrefixes as OpenAI ships them.
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
