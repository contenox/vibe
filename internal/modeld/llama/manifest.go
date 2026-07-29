package llama

import "github.com/contenox/contenox/internal/kernel/contextasm"

// The llama backend keys warm KV reuse on the backend-neutral context manifest
// owned by the runtime (runtime/contextasm, surfaced to the runtime as
// transport.ContextManifest). These aliases let the llama.cpp session adapter
// and its tests refer to those types through this package without importing
// contextasm directly. The manifest is assembled by the runtime and crosses the
// transport; modeld only fills the backend-resolved token data during prefill.
type (
	ContextManifest       = contextasm.ContextManifest
	ManifestSegment       = contextasm.ManifestSegment
	TokenizeFunc          = contextasm.TokenizeFunc
	ManifestMismatchError = contextasm.ManifestMismatchError
)

// ErrManifestMismatch is returned when a prefix/suffix cannot be safely paired
// with resident KV under the current manifest.
var ErrManifestMismatch = contextasm.ErrManifestMismatch

// NewManifestMismatchError builds a manifest-mismatch error with a reason.
func NewManifestMismatchError(reason string) error {
	return contextasm.NewManifestMismatchError(reason)
}
