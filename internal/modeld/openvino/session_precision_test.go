package openvino

import (
	"context"
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

func TestUnit_KVPrecisionLossless(t *testing.T) {
	for _, p := range []string{"", "f16", "F16", "fp16", "f32", "fp32", "  f16  "} {
		if !kvPrecisionLossless(p) {
			t.Errorf("kvPrecisionLossless(%q) = false, want true (float precision round-trips)", p)
		}
	}
	for _, p := range []string{"q8_0", "q4_0", "q4_1", "int8", "u8"} {
		if kvPrecisionLossless(p) {
			t.Errorf("kvPrecisionLossless(%q) = true, want false (quantized precision is lossy)", p)
		}
	}
}

// TestGenaiSessionColdStoreDisabledAtLossyKVPrecision proves a cold-capable
// session refuses the cold-KV path when its KV precision cannot round-trip raw KV
// bytes, so an evicted-then-readmitted block can never silently degrade. The cold
// path is otherwise enabled (backend supports cold + plannerCtx > numCtx).
func TestGenaiSessionColdStoreDisabledAtLossyKVPrecision(t *testing.T) {
	fake := &fakeGenAIBackend{supportsColdKV: true}
	s := newGenaiSessionWithPlanner(fake, 6, 10, true)

	// Control: the default (lossless f16) keeps cold enabled.
	if !s.Capabilities().ColdStore {
		t.Fatal("control: lossless session should advertise ColdStore")
	}

	// Lossy precision (as OpenSession sets from e.g. a q8_0 profile) disables it.
	s.coldKVLossless = false
	caps := s.Capabilities()
	if caps.ColdStore || caps.RemoveTail || caps.RemoveMiddle || caps.PositionShift {
		t.Fatalf("lossy session must not advertise cold-dependent caps: %+v", caps)
	}

	m := ovManifest(contextasm.HashString("abcdef"), "r1")
	if _, err := s.EnsurePrefix(context.Background(), transport.PrefixInput{Text: "abcdef", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	err := s.EvictRange(context.Background(), residency.Range{Start: 0, End: 2})
	if !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("EvictRange on lossy session = %v, want ErrUnsupportedFeature", err)
	}
}
