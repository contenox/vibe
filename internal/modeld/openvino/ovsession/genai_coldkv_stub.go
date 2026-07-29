//go:build !openvino || !openvino_genai

package ovsession

import "context"

// SupportsColdKV reports whether this GenAI bridge can import/export cold KV
// blocks. The stock no-CGO bridge cannot.
func (s *GenAISession) SupportsColdKV() bool { return false }

// ExportColdKV exports a logical token range from OpenVINO hot KV.
func (s *GenAISession) ExportColdKV(context.Context, ColdKVRange) ([]byte, error) {
	return nil, ErrColdKVUnsupported
}

// ImportColdKV imports a previously exported logical token range into OpenVINO
// hot KV.
func (s *GenAISession) ImportColdKV(context.Context, ColdKVRange, []byte) error {
	return ErrColdKVUnsupported
}
