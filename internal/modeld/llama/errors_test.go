package llama_test

import (
	"errors"
	"testing"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/libtransport"
)

// TestBoundaryErrorsMatchTransportSentinels pins the llama backend's boundary
// errors to the transport sentinels the modeld wire map keys on.
//
// The gRPC boundary (runtime/transport/grpc/errors.go) maps a backend error to a
// stable wire code by testing errors.Is against the transport.Err* sentinels. A
// llama error that is not Is-compatible with its matching transport sentinel is
// downgraded to codes.Internal, so the client cannot classify it — the same
// failure the OpenVINO backend does not have because it returns the transport
// sentinels directly. These cases assert the daemon emits transport-recoverable
// errors, mirroring how manifest.go already aliases contextasm.ErrManifestMismatch.
func TestBoundaryErrorsMatchTransportSentinels(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"session closed", llama.ErrSessionClosed, transport.ErrSessionClosed},
		{"context overflow", llama.NewContextOverflowError("suffix", 10, 4, 12), transport.ErrContextOverflow},
		{"unsupported feature", llama.NewUnsupportedFeatureError("tool calls"), transport.ErrUnsupportedFeature},
		{"session fatal", llama.ErrSessionFatal, transport.ErrSessionFatal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false; the modeld wire map would downgrade this to codes.Internal", tc.err, tc.sentinel)
			}
		})
	}
}
