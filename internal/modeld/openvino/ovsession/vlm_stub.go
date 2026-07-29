//go:build !openvino || !openvino_genai

package ovsession

import (
	"context"
	"errors"
)

// VLMAvailable reports whether the OpenVINO VLM (vision) backend was built.
const VLMAvailable = false

// VLMImage is one encoded image file attached to a VLM generation. It mirrors
// transport.ImagePart.
type VLMImage struct {
	Data     []byte
	MimeType string
}

// ProbeVLMImage reports that the native VLM backend is not compiled in.
func ProbeVLMImage(_ []byte) (int, int, error) {
	return 0, 0, errors.New("openvino GenAI backend not compiled")
}

// VLMSession is unavailable without the openvino and openvino_genai build tags.
type VLMSession struct{}

// NewVLM reports that the native VLM backend is not compiled in.
func NewVLM(_, _ string) (*VLMSession, error) {
	return nil, errors.New("openvino GenAI backend not compiled")
}

// ApplyChatTemplate reports that the native VLM backend is not compiled in.
func (s *VLMSession) ApplyChatTemplate(_ []ChatMessage, _ bool) (string, error) {
	return "", errors.New("openvino GenAI backend not compiled")
}

// Tokenize reports that the native VLM backend is not compiled in.
func (s *VLMSession) Tokenize(_ context.Context, _ string, _ bool) ([]int, error) {
	return nil, errors.New("openvino GenAI backend not compiled")
}

// Stream reports that the native VLM backend is not compiled in.
func (s *VLMSession) Stream(_ context.Context, _ string, _ []VLMImage, _ GenerateOptions) (<-chan StreamChunk, error) {
	return nil, errors.New("openvino GenAI backend not compiled")
}

// Generate reports that the native VLM backend is not compiled in.
func (s *VLMSession) Generate(_ context.Context, _ string, _ []VLMImage, _ GenerateOptions) (string, error) {
	return "", errors.New("openvino GenAI backend not compiled")
}

// Close is a no-op for the stub.
func (s *VLMSession) Close() error { return nil }
