// Package modelstore resolves model names to on-disk paths within a modeld
// node's models directory. It exists so a session request can omit Path: a
// remote node has no idea what filesystem layout the runtime that sent the
// request uses, so the node must resolve ModelName+Type against its own
// storage instead of trusting a caller-supplied path.
//
// Layout mirrors what the runtime's local catalog providers already scan
// (runtime/modelrepo/llama, runtime/modelrepo/openvino), so an existing local
// models directory works unchanged as a node's models dir:
//
//	<dir>/<name>/model.gguf              (llama)
//	<dir>/<name>/openvino_model.xml       (openvino, or openvino_language_model.xml)
package modelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/transport"
)

// DefaultSubdir is the models directory name under a data root when no
// explicit override is configured.
const DefaultSubdir = "models"

var openvinoEntrypoints = []string{"openvino_model.xml", "openvino_language_model.xml"}

// These alias the transport package's canonical sentinels rather than
// defining new error values: model resolution errors surface both in-process
// (slot.Service callers checking modelstore.ErrModelNotFound directly) and
// across the gRPC wire (Admin.ReceiveModel's errors, encoded/decoded via
// transport/grpc's sentinel table). A local, unrelated error value here would
// lose its identity crossing the wire.
var (
	// ErrModelNotFound is returned when no model matching the requested
	// name+type exists in the models directory.
	ErrModelNotFound = transport.ErrModelNotFound
	// ErrDigestMismatch is returned when a caller-supplied digest does not
	// match the on-disk content. Only enforced when the caller supplies a
	// non-empty digest to verify against.
	ErrDigestMismatch = transport.ErrDigestMismatch
	// ErrUnsupportedType is returned for a backend type this package does not
	// know how to resolve.
	ErrUnsupportedType = transport.ErrUnsupportedModelType
)

// Dir resolves the models directory: an explicit override if non-empty,
// otherwise <dataRoot>/<DefaultSubdir>.
func Dir(dataRoot, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(dataRoot, DefaultSubdir)
}

// Resolve finds the on-disk path for a model by name and backend type within
// dir. When wantDigest is non-empty and the backend type supports digest
// verification, the resolved file's content digest is compared and a
// mismatch is rejected — this guards against a stale or wrong file answering
// under a name the caller expects to be a specific model.
//
// OpenVINO IR models are directories; digest verification for them is not
// yet implemented (their content-addressing arrives with node-side push in a
// later phase), so wantDigest is ignored for backend type "openvino".
func Resolve(dir, name, backendType, wantDigest string) (path string, err error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty model name", ErrModelNotFound)
	}
	modelDir := filepath.Join(dir, name)

	switch backendType {
	case "llama":
		path = filepath.Join(modelDir, "model.gguf")
		if _, statErr := os.Stat(path); statErr != nil {
			return "", fmt.Errorf("%w: %s (llama)", ErrModelNotFound, name)
		}
		if wantDigest != "" {
			got, digestErr := FileDigest(path)
			if digestErr != nil {
				return "", digestErr
			}
			if got != wantDigest {
				return "", fmt.Errorf("%w: model %s want=%s got=%s", ErrDigestMismatch, name, wantDigest, got)
			}
		}
		return path, nil

	case "openvino":
		for _, entry := range openvinoEntrypoints {
			if _, statErr := os.Stat(filepath.Join(modelDir, entry)); statErr == nil {
				return modelDir, nil
			}
		}
		return "", fmt.Errorf("%w: %s (openvino)", ErrModelNotFound, name)

	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedType, backendType)
	}
}

// ResolveMMProj returns the optional multimodal projector installed next to a
// resolved llama model path (see llama.MMProjFilename), or "" when the model
// has none. The projector is deliberately not part of Resolve's digest check:
// the model's cache identity stays the model.gguf content digest, and vision
// capability is certified separately by the backend's Describe.
func ResolveMMProj(modelPath string) string {
	return llama.MMProjPathFor(modelPath)
}

// FileDigest returns the hex-encoded sha256 digest of a file's content.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("modelstore: open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("modelstore: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
