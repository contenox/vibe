package llama

import (
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
)

// MMProjFilename is the multimodal projector file expected next to a llama
// model's model.gguf (<models>/<name>/mmproj.gguf). The llama backend owns
// this two-file layout convention: modelstore resolution, capacity planning
// (Describe), and session open all derive the projector location from the
// resolved model path via MMProjPathFor, so a vision model installs, plans,
// and serves from one naming rule.
const MMProjFilename = "mmproj.gguf"

// MMProjPathFor returns the projector path sitting next to a resolved llama
// model file, or "" when the model has none (a text-only model).
func MMProjPathFor(modelPath string) string {
	if modelPath == "" {
		return ""
	}
	p := filepath.Join(filepath.Dir(modelPath), MMProjFilename)
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return ""
	}
	return p
}

// Native seams for the projector metadata reads, overridable in unit tests
// like inspectLlamaModel.
var (
	mmprojCaps           = llamacppshim.MMProjCaps
	inspectMMProjProfile = llamacppshim.InspectMMProjProfile
)

// defaultVisionEncoderReserveBytes is the compute-buffer reserve budgeted for
// the vision encoder graph (image preprocessing + projector forward pass).
// Like the GPU compute reserve it is a planning constant, not a measurement;
// operators tune it with CONTENOX_LLAMA_VISION_COMPUTE_RESERVE.
const defaultVisionEncoderReserveBytes int64 = 320 << 20

func visionEncoderReserveBytes() int64 {
	if v, err := capacity.ParseBytes(os.Getenv("CONTENOX_LLAMA_VISION_COMPUTE_RESERVE")); err == nil && v > 0 {
		return v
	}
	return defaultVisionEncoderReserveBytes
}

// visionTokensPerImageEstimate is the conservative sequence-token cost of one
// image after encoding: the full patch grid, folded by the projector's
// pixel-shuffle merge when the metadata declares one. It intentionally ignores
// dynamic-resolution slicing (the engine caps that via its image token limit),
// so it is a planning estimate — the session's typed context-overflow refusal
// stays the hard gate. 0 means the metadata was insufficient to estimate.
func visionTokensPerImageEstimate(p llamacppshim.MMProjProfile) int {
	if p.ImageSize <= 0 || p.PatchSize <= 0 {
		return 0
	}
	grid := p.ImageSize / p.PatchSize
	tokens := grid * grid
	if p.ProjScaleFactor > 1 {
		tokens /= p.ProjScaleFactor * p.ProjScaleFactor
	}
	if tokens <= 0 {
		return 0
	}
	return tokens
}
