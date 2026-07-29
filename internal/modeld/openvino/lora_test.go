package openvino

import (
	"testing"

	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
	"github.com/contenox/contenox/internal/transport"
)

// toGenAILoRA is the service-boundary mapper from the transport adapter handle to
// OpenVINO's safetensors adapter config. It must preserve order and map the
// transport Scale onto OpenVINO's folded Alpha, and treat empty as the base model.
func TestUnit_ToGenAILoRA_MapsScaleToAlphaInOrder(t *testing.T) {
	if got := toGenAILoRA(nil); got != nil {
		t.Fatalf("empty adapters should map to nil (base model), got %+v", got)
	}

	in := []transport.AdapterSpec{
		{Name: "a", Path: "/a.safetensors", Digest: "da", Scale: 1.5},
		{Name: "b", Path: "/b.safetensors", Digest: "db", Scale: 2},
	}
	want := []ovsession.GenAILoRAAdapter{
		{Path: "/a.safetensors", Alpha: 1.5},
		{Path: "/b.safetensors", Alpha: 2},
	}
	got := toGenAILoRA(in)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("adapter %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
