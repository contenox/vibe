package openvino

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// An exported OpenVINO VLM directory replaces openvino_model.xml with
// openvino_language_model.xml and adds vision/text embedding IR pairs (plus an
// optional resampler). The OpenVINO backend owns this layout rule: modelstore
// resolves either entrypoint (read-only for this package), while Describe and
// OpenSession use the joint presence of the language + vision-embeddings IRs
// to certify vision support from the model's own files — never from its name.
const (
	openvinoLanguageModelXML    = "openvino_language_model.xml"
	openvinoVisionEmbeddingsXML = "openvino_vision_embeddings_model.xml"
)

// isVLMModelDir reports whether the resolved OpenVINO IR directory is an
// exported vision-language model.
func isVLMModelDir(dir string) bool {
	if dir == "" {
		return false
	}
	for _, name := range []string{openvinoLanguageModelXML, openvinoVisionEmbeddingsXML} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// openvinoVisionConfig is the subset of a VLM export's config.json used to
// estimate the sequence-token cost of one image.
type openvinoVisionConfig struct {
	// Gemma 3 exports declare the folded per-image token count directly.
	MMTokensPerImage int `json:"mm_tokens_per_image"`
	// Gemma 4 exports call the same quantity vision_soft_tokens_per_image.
	VisionSoftTokensPerImage int `json:"vision_soft_tokens_per_image"`
	// InternVL-family exports declare the tile geometry instead.
	ForceImageSize  int     `json:"force_image_size"`
	DownsampleRatio float64 `json:"downsample_ratio"`
	VisionConfig    struct {
		ImageSize int `json:"image_size"`
		PatchSize int `json:"patch_size"`
	} `json:"vision_config"`
}

// visionTokensPerImageEstimate is the conservative sequence-token cost of one
// image after encoding, read from the VLM export's own config.json: the
// declared per-image count when the export states one (gemma3/gemma4), else
// the base patch grid folded by the declared downsample ratio (InternVL). It
// intentionally ignores dynamic-resolution tiling, mirroring the llama
// backend's estimate policy — it is a planning number, and the session's typed
// context-overflow refusal stays the hard gate. 0 means the config was
// insufficient to estimate.
func visionTokensPerImageEstimate(modelDir string) int {
	raw, err := os.ReadFile(filepath.Join(modelDir, "config.json"))
	if err != nil {
		return 0
	}
	var cfg openvinoVisionConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0
	}
	if cfg.MMTokensPerImage > 0 {
		return cfg.MMTokensPerImage
	}
	if cfg.VisionSoftTokensPerImage > 0 {
		return cfg.VisionSoftTokensPerImage
	}
	imageSize := cfg.ForceImageSize
	if imageSize <= 0 {
		imageSize = cfg.VisionConfig.ImageSize
	}
	if imageSize <= 0 || cfg.VisionConfig.PatchSize <= 0 {
		return 0
	}
	grid := imageSize / cfg.VisionConfig.PatchSize
	tokens := grid * grid
	if cfg.DownsampleRatio > 0 && cfg.DownsampleRatio < 1 {
		tokens = int(float64(tokens) * cfg.DownsampleRatio * cfg.DownsampleRatio)
	}
	if tokens <= 0 {
		return 0
	}
	return tokens
}
