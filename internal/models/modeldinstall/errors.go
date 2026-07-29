package modeldinstall

import "errors"

// Internal errors callers branch on with errors.Is. All except ErrChecksumMismatch
// are "soft" — setup should fall back to source-build instructions. A checksum
// mismatch is a hard failure: the download must not be treated as merely
// unavailable.
var (
	// ErrNoIndex: the public release index is missing, inaccessible, or not
	// readable by anonymous setup clients.
	ErrNoIndex = errors.New("modeld setup: no public release index")
	// ErrNoCompatibleArtifact: the index has no stable build for this platform,
	// provider backend, and transport protocol window.
	ErrNoCompatibleArtifact = errors.New("modeld setup: no compatible prebuilt artifact")
	// ErrArtifactUnavailable: the index selected an artifact whose archive or
	// checksum object could not be fetched.
	ErrArtifactUnavailable = errors.New("modeld setup: selected artifact is unavailable")
	// ErrChecksumMismatch: the archive did not match its published checksum.
	ErrChecksumMismatch = errors.New("modeld setup: checksum mismatch")
	// ErrUnsupportedPlatform: this GOOS has no published package format.
	ErrUnsupportedPlatform = errors.New("modeld setup: unsupported platform")
	// ErrProtocolMismatch: the installed modeld speaks a transport protocol this
	// runtime build does not support.
	ErrProtocolMismatch = errors.New("modeld setup: incompatible modeld transport protocol")
	// ErrBackendMissing: the installed modeld lacks the compiled backend the
	// selected provider needs.
	ErrBackendMissing = errors.New("modeld setup: installed modeld lacks required compiled backend")
)
