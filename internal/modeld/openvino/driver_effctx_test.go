package openvino

import (
	"context"
	"testing"

	"github.com/contenox/contenox/internal/kernel/contextasm"
	"github.com/contenox/contenox/internal/transport"
)

// TestUnit_OpenVINO_DriveEvictToFit_ParksVolatileToCold proves the residency
// driver: when an incoming prefill would overflow the hot window, evictable
// (volatile) context is parked to the cold store while the pinned system prompt
// stays hot — instead of overflowing or dropping it. This is the pure-Go core of
// the effective-context driver; the real-backend round-trip is the system test.
func TestUnit_OpenVINO_DriveEvictToFit_ParksVolatileToCold(t *testing.T) {
	fake := &fakeGenAIBackend{supportsColdKV: true}
	s := newGenaiSessionWithPlanner(fake, 10, 16, false) // numCtx=10, plannerCtx=16 -> coldMax=6
	if !s.coldEnabledLocked() {
		t.Fatal("precondition: cold store should be enabled")
	}
	// Resident tape: pinned system [0,2) + volatile user [2,8) = 8 hot tokens.
	s.resident = []int{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'}
	s.prefixLen = 2
	s.manifest = transport.ContextManifest{
		Segments: []contextasm.ManifestSegment{
			{Kind: "system", Stable: true, TokenStart: 0, TokenEnd: 2},
			{Kind: "user", Stable: false, TokenStart: 2, TokenEnd: 8},
		},
	}

	// A 6-token suffix overflows numCtx (8+6=14>10). Drive parks the volatile
	// range to cold so the suffix fits, keeping the pinned system prompt hot.
	if err := s.driveEvictToFitLocked(context.Background(), 6); err != nil {
		t.Fatalf("driveEvictToFitLocked: %v", err)
	}
	if got := s.residentTokens() + 6; got > s.numCtx {
		t.Fatalf("after drive, resident(%d)+incoming(6)=%d still exceeds numCtx %d", s.residentTokens(), got, s.numCtx)
	}
	if s.coldTokens == 0 {
		t.Fatal("expected volatile context parked to cold, got coldTokens=0")
	}
	if s.residentTokens() != 2 {
		t.Fatalf("resident after parking = %d, want 2 (pinned system kept hot)", s.residentTokens())
	}
}

// TestUnit_OpenVINO_DriveEvictToFit_NoopWhenFits proves the driver does nothing
// when the incoming tokens already fit the hot window.
func TestUnit_OpenVINO_DriveEvictToFit_NoopWhenFits(t *testing.T) {
	fake := &fakeGenAIBackend{supportsColdKV: true}
	s := newGenaiSessionWithPlanner(fake, 100, 200, false)
	s.resident = []int{1, 2, 3, 4}
	s.manifest = transport.ContextManifest{
		Segments: []contextasm.ManifestSegment{
			{Kind: "user", Stable: false, TokenStart: 0, TokenEnd: 4},
		},
	}
	if err := s.driveEvictToFitLocked(context.Background(), 4); err != nil {
		t.Fatalf("driveEvictToFitLocked: %v", err)
	}
	if s.coldTokens != 0 {
		t.Fatalf("expected no parking when the suffix already fits, coldTokens=%d", s.coldTokens)
	}
}
