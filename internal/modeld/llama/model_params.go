package llama

import (
	"fmt"

	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
)

// ggufParams are the architecture facts modeld needs to size KV cache and the
// context ceiling. Zero means absent.
type ggufParams struct {
	ContextLength   int
	BlockCount      int // transformer layers
	HeadCountKV     int // KV heads (GQA); falls back to HeadCount
	HeadCount       int
	KeyLength       int // per-head dim, when declared
	EmbeddingLength int
	SlidingWindow   int // model-native sliding-window attention size (SWA)
	// SlidingWindowPattern marks per-layer attention type when present. true
	// means the layer uses sliding-window attention; false means global.
	SlidingWindowPattern       []bool
	SlidingWindowPatternStride int // legacy scalar: every nth layer is global
}

// inspectLlamaModel is the production metadata seam. It is overridden in unit
// tests; production reads through llamacppshim, not a Go-side GGUF parser.
var inspectLlamaModel = inspectLlamaModelParams

func inspectLlamaModelParams(path string) (ggufParams, error) {
	profile, err := llamacppshim.InspectModelKVProfile(path)
	if err != nil {
		return ggufParams{}, fmt.Errorf("llama model KV profile: %w", err)
	}
	return ggufParams{
		ContextLength:              profile.ContextLength,
		BlockCount:                 profile.BlockCount,
		HeadCountKV:                profile.HeadCountKV,
		HeadCount:                  profile.HeadCount,
		KeyLength:                  profile.KeyLength,
		EmbeddingLength:            profile.EmbeddingLength,
		SlidingWindow:              profile.SlidingWindow,
		SlidingWindowPattern:       append([]bool(nil), profile.SlidingWindowPattern...),
		SlidingWindowPatternStride: profile.SlidingWindowPatternStride,
	}, nil
}

// ContextLength returns a llama GGUF model's trained context ceiling by
// reading only file metadata (no tensor data, no device query) — the cheap
// header-only part of what Describe's capacity planner already computes.
// Callers (e.g. modelstore.Admin.ListModels) should treat an error as
// "unknown" and skip enrichment for that one model, not fail the scan.
func ContextLength(ggufPath string) (int, error) {
	p, err := inspectLlamaModel(ggufPath)
	if err != nil {
		return 0, err
	}
	return p.ContextLength, nil
}

// headDim is the per-head KV dimension: the declared key_length, else
// embedding_length / head_count.
func (p ggufParams) headDim() int {
	if p.KeyLength > 0 {
		return p.KeyLength
	}
	if p.HeadCount > 0 && p.EmbeddingLength > 0 {
		return p.EmbeddingLength / p.HeadCount
	}
	return 0
}

// kvHeads is the number of KV heads (head_count_kv for GQA, else head_count).
func (p ggufParams) kvHeads() int {
	if p.HeadCountKV > 0 {
		return p.HeadCountKV
	}
	return p.HeadCount
}

func (p ggufParams) layerSplit() (global, windowed int) {
	if p.BlockCount <= 0 {
		return 0, 0
	}
	if p.SlidingWindow <= 0 {
		return p.BlockCount, 0
	}
	if len(p.SlidingWindowPattern) > 0 {
		n := min(len(p.SlidingWindowPattern), p.BlockCount)
		for i := 0; i < n; i++ {
			if p.SlidingWindowPattern[i] {
				windowed++
			} else {
				global++
			}
		}
		global += p.BlockCount - n
		return global, windowed
	}
	if p.SlidingWindowPatternStride > 0 {
		global = (p.BlockCount + p.SlidingWindowPatternStride - 1) / p.SlidingWindowPatternStride
		if global > p.BlockCount {
			global = p.BlockCount
		}
		return global, p.BlockCount - global
	}
	return 0, p.BlockCount
}
