package grpc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/contenox/contenox/internal/kernel/contextasm"
)

// TestSentinelTableRoundTrips asserts every sentinel in the wire map survives
// encodeError -> status -> decodeError as an errors.Is-recoverable value. A
// sentinel missing from the table (or a backend error not Is-compatible with
// one) degrades to codes.Internal and loses its class at the boundary, so this
// guards the table as the single source of truth for what crosses the wire.
func TestSentinelTableRoundTrips(t *testing.T) {
	for _, s := range sentinels {
		t.Run(s.token, func(t *testing.T) {
			wire := encodeError(fmt.Errorf("%w: detail", s.err))
			got := decodeError(wire)
			if !errors.Is(got, s.err) {
				t.Fatalf("token %q round-trip = %v, want errors.Is %v", s.token, got, s.err)
			}
		})
	}
}

func TestManifestMismatchRoundTripDoesNotDuplicatePrefix(t *testing.T) {
	wire := encodeError(contextasm.NewManifestMismatchError("stable prefix changed"))
	got := decodeError(wire)
	if !errors.Is(got, contextasm.ErrManifestMismatch) {
		t.Fatalf("round-trip = %v, want ErrManifestMismatch", got)
	}
	if got.Error() != "contextasm: context manifest mismatch: stable prefix changed" {
		t.Fatalf("round-trip error = %q", got.Error())
	}
}
