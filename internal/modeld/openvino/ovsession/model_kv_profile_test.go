//go:build openvino && openvino_genai

package ovsession

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnit_InspectModelKVProfile_LayerTypes(t *testing.T) {
	dir := t.TempDir()
	cfg := []byte(`{
		"max_position_embeddings": 131072,
		"num_hidden_layers": 6,
		"num_key_value_heads": 4,
		"num_attention_heads": 8,
		"head_dim": 512,
		"sliding_window": 512,
		"layer_types": [
			"sliding_attention",
			"sliding_attention",
			"full_attention",
			"sliding_attention",
			"sliding_attention",
			"full_attention"
		]
	}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := InspectModelKVProfile(dir)
	if err != nil {
		t.Fatalf("InspectModelKVProfile: %v", err)
	}
	if got.MaxPositionEmbeddings != 131072 || got.SlidingWindow != 512 ||
		got.GlobalLayers != 2 || got.WindowedLayers != 4 {
		t.Fatalf("profile = %+v, want ctx=131072 window=512 global/windowed=2/4", got)
	}
}

func TestUnit_InspectModelKVProfile_TextConfigStridePattern(t *testing.T) {
	dir := t.TempDir()
	cfg := []byte(`{
		"text_config": {
			"max_position_embeddings": 131072,
			"num_hidden_layers": 42,
			"num_key_value_heads": 4,
			"num_attention_heads": 8,
			"hidden_size": 4096,
			"sliding_window": 512,
			"sliding_window_pattern": 6
		}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := InspectModelKVProfile(dir)
	if err != nil {
		t.Fatalf("InspectModelKVProfile: %v", err)
	}
	if got.GlobalLayers != 7 || got.WindowedLayers != 35 {
		t.Fatalf("profile = %+v, want global/windowed=7/35", got)
	}
}
