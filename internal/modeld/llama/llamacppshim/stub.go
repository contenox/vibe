//go:build !llamacpp_direct

package llamacppshim

import "errors"

// Available reports whether the direct llama.cpp shim was compiled in.
const Available = false

// ModelKVProfile contains model metadata needed to budget KV cache.
type ModelKVProfile struct {
	ContextLength              int
	BlockCount                 int
	HeadCountKV                int
	HeadCount                  int
	KeyLength                  int
	EmbeddingLength            int
	SlidingWindow              int
	SlidingWindowPattern       []bool
	SlidingWindowPatternStride int
}

// InspectModelKVProfile is available only in direct llama.cpp builds.
func InspectModelKVProfile(string) (ModelKVProfile, error) {
	return ModelKVProfile{}, errors.New("llamacppshim: direct llama.cpp backend is not compiled in")
}

// ChatTemplateProbe reports what the linked llama.cpp common_chat engine
// detects from a model's own chat template.
type ChatTemplateProbe struct {
	FormatName              string
	ThinkingStartTag        string
	SupportsToolCalls       bool
	SupportsThinking        bool
	SupportsReasoningEffort bool
}

// ProbeChatTemplate is available only in direct llama.cpp builds.
func ProbeChatTemplate(string) (ChatTemplateProbe, error) {
	return ChatTemplateProbe{}, errors.New("llamacppshim: direct llama.cpp backend is not compiled in")
}

// MMProjCaps reports projector input modalities only in direct llama.cpp
// builds; without the native backend no capability can be certified.
func MMProjCaps(string) (vision, audio bool) {
	return false, false
}

// MMProjProfile contains the multimodal projector metadata needed to estimate
// per-image token cost.
type MMProjProfile struct {
	ImageSize       int
	PatchSize       int
	ProjScaleFactor int
}

// InspectMMProjProfile is available only in direct llama.cpp builds.
func InspectMMProjProfile(string) (MMProjProfile, error) {
	return MMProjProfile{}, errors.New("llamacppshim: direct llama.cpp backend is not compiled in")
}
