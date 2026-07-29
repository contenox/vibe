//go:build !openvino || !openvino_legacy_shim

package ovsession

import (
	"errors"
	"fmt"
)

// Available reports whether the legacy InferRequest OpenVINO shim was compiled in.
const Available = false

// ErrUnavailable is returned by the stub implementation.
var ErrUnavailable = errors.New("openvino legacy native backend is not built; rebuild with -tags 'openvino openvino_legacy_shim'")

// Session is a placeholder in default builds.
type Session struct{}

// New returns ErrUnavailable in default builds.
func New(modelDir, device string) (*Session, error) {
	return nil, fmt.Errorf("%w (model_dir=%q device=%q)", ErrUnavailable, modelDir, device)
}

func (s *Session) Close() error                      { return nil }
func (s *Session) Prefill(tokens []int64) error      { return ErrUnavailable }
func (s *Session) DecodeNext() (int64, error)        { return -1, ErrUnavailable }
func (s *Session) SnapshotSave(path string) error    { return ErrUnavailable }
func (s *Session) SnapshotRestore(path string) error { return ErrUnavailable }
func (s *Session) SnapshotData() ([]byte, error)     { return nil, ErrUnavailable }
func (s *Session) SnapshotRestoreData([]byte) error  { return ErrUnavailable }
