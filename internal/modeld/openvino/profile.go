package openvino

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
)

// genAIConfigFromProfile builds the GenAI session config from the model's own
// contenox-openvino.json beside the IR. KV-cache precision, prefix caching,
// sparse attention, XAttention, and cache size are model-driven. Missing or
// partial profiles take conservative defaults; sparse attention and other
// aggressive knobs are only enabled when the profile asks.
func genAIConfigFromProfile(modelDir, device string) ovsession.GenAIConfig {
	var p struct {
		Device string `json:"device"`
		GenAI  struct {
			KVCachePrecision            string  `json:"kv_cache_precision"`
			CacheSize                   *int    `json:"cache_size"`
			DynamicSplitFuse            *bool   `json:"dynamic_split_fuse"`
			EnablePrefixCaching         *bool   `json:"enable_prefix_caching"`
			UseSparseAttention          *bool   `json:"use_sparse_attention"`
			NumLastDenseTokensInPrefill int     `json:"num_last_dense_tokens_in_prefill"`
			XAttentionThreshold         float32 `json:"xattention_threshold"`
			XAttentionBlockSize         int     `json:"xattention_block_size"`
			XAttentionStride            int     `json:"xattention_stride"`
		} `json:"genai"`
	}
	if b, err := os.ReadFile(filepath.Join(modelDir, "contenox-openvino.json")); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	g := p.GenAI
	fallbackDevice := strings.TrimSpace(device)
	switch profileDevice := strings.TrimSpace(p.Device); {
	case profileDevice != "":
		device = profileDevice
	case fallbackDevice != "":
		device = fallbackDevice
	default:
		device = "AUTO"
	}
	cfg := ovsession.GenAIConfig{
		Device:                      device,
		KVCachePrecision:            g.KVCachePrecision,
		DynamicSplitFuse:            g.DynamicSplitFuse,
		EnablePrefixCaching:         g.EnablePrefixCaching,
		UseSparseAttention:          g.UseSparseAttention,
		NumLastDenseTokensInPrefill: g.NumLastDenseTokensInPrefill,
		XAttentionThreshold:         g.XAttentionThreshold,
		XAttentionBlockSize:         g.XAttentionBlockSize,
		XAttentionStride:            g.XAttentionStride,
	}
	if g.CacheSize != nil && *g.CacheSize > 0 {
		cfg.CacheSize = *g.CacheSize
		cfg.CacheSizeExplicit = true
	}
	if cfg.KVCachePrecision == "" {
		cfg.KVCachePrecision = "f16"
	}
	if cfg.EnablePrefixCaching == nil {
		enabled := true
		cfg.EnablePrefixCaching = &enabled
	}
	return cfg
}
