// Package openvino implements the runtime/transport.Service boundary for the
// OpenVINO (Intel) backend: it opens persistent, manifest-keyed sessions on the
// owned device (CPU / GPU) that the runtime drives over the transport. It
// is not a modelprovider — the stateless modelprovider lives in the runtime; this
// package is the daemon-side compute implementation.
//
// session.go adapts OpenVINO GenAI (string-prompt based, with the tokenizer,
// chat template, and prefix cache held inside the ContinuousBatchingPipeline) to
// the EnsurePrefix/PrefillSuffix/Decode contract. The native backend lives in the
// isolated sub-package ./ovsession behind the 'openvino' and 'openvino_genai'
// build tags, so the default build and CI never require OpenVINO or a C++
// toolchain; without the tags ovsession reports Available == false and
// OpenSession returns the not-compiled-in error.
//
// Build and benchmark the native path with Makefile.openvino (the CGO flags are
// derived from the OpenVINO wheels; CONTENOX_OPENVINO_DEVICE selects CPU/GPU).
package openvino
