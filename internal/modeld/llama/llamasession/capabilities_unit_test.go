package llamasession

import (
	"testing"

	"github.com/contenox/contenox/internal/modeld/residency"
)

// TestUnit_LlamaCapabilitiesVerbatim pins the full capability struct so drift
// fails plain CI (backend-parity contract I2): a capability is reported if and
// only if the session can execute it. The mapping is pure and cgo-free, so
// this runs without the native llama.cpp build tags.
func TestUnit_LlamaCapabilitiesVerbatim(t *testing.T) {
	hot := capabilitiesFor(false, 0, 0)
	want := residency.Capabilities{
		RemoveTail:    true,
		RemoveMiddle:  true,
		PositionShift: true,
	}
	if hot != want {
		t.Fatalf("hot capabilities = %+v, want %+v", hot, want)
	}

	cold := capabilitiesFor(true, 512, 1024)
	want = residency.Capabilities{
		RemoveTail:                   true,
		RemoveMiddle:                 true,
		PositionShift:                true,
		SparseAttention:              true,
		SlidingWindowAttentionTokens: 512,
		ColdStore:                    true,
	}
	if cold != want {
		t.Fatalf("cold capabilities = %+v, want %+v", cold, want)
	}
}
