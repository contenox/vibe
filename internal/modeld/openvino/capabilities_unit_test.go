package openvino

import (
	"testing"

	"github.com/contenox/contenox/internal/modeld/residency"
)

// TestUnit_OpenvinoCapabilitiesVerbatim pins the full capability struct per
// cold-KV configuration so drift fails plain CI (backend-parity contract I2).
// Capabilities report ability per the transport contract: the adapter owns
// the full token tape, so recompute is always executable; the KV-surgery
// capabilities appear exactly when the lossless cold path is active.
func TestUnit_OpenvinoCapabilitiesVerbatim(t *testing.T) {
	recomputeOnly := residency.Capabilities{
		SparseAttention: true,
		RecomputeRange:  true,
	}
	coldSurgery := residency.Capabilities{
		RemoveTail:      true,
		RemoveMiddle:    true,
		PositionShift:   true,
		SparseAttention: true,
		ColdStore:       true,
		RecomputeRange:  true,
	}

	cases := []struct {
		name string
		sess *genaiSession
		want residency.Capabilities
	}{
		{
			name: "no cold budget: recompute only",
			sess: newGenaiSessionWithNativeFeatures(&fakeGenAIBackend{}, 4096, 4096, false, true, 0),
			want: recomputeOnly,
		},
		{
			name: "lossless cold path adds KV surgery",
			sess: newGenaiSessionWithPlanner(&fakeGenAIBackend{supportsColdKV: true}, 6, 10, true),
			want: coldSurgery,
		},
		{
			name: "backend without cold hooks: recompute only",
			sess: newGenaiSessionWithPlanner(&fakeGenAIBackend{}, 6, 10, true),
			want: recomputeOnly,
		},
	}
	for _, tc := range cases {
		if got := tc.sess.Capabilities(); got != tc.want {
			t.Fatalf("%s: capabilities = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// A quantized KV precision cannot round-trip cold blocks: the surgery
	// capabilities disappear, recompute remains.
	lossy := newGenaiSessionWithPlanner(&fakeGenAIBackend{supportsColdKV: true}, 6, 10, true)
	lossy.coldKVLossless = false
	if got := lossy.Capabilities(); got != recomputeOnly {
		t.Fatalf("lossy KV precision: capabilities = %+v, want %+v", got, recomputeOnly)
	}
}
