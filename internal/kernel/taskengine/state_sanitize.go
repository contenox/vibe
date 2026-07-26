package taskengine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

const (
	capturedPayloadMaxJSONBytes = 32 * 1024
	capturedPayloadPreviewBytes = 4 * 1024
	capturedErrorMaxBytes       = 8 * 1024
)

// CapturedPayloadSummary replaces oversized or non-JSON-marshallable captured
// payloads in persisted/streamed state. In-memory execution history keeps the
// original values; this type is for observability storage only.
type CapturedPayloadSummary struct {
	Truncated         bool   `json:"truncated"`
	Reason            string `json:"reason"`
	OriginalType      string `json:"originalType,omitempty"`
	OriginalJSONBytes int    `json:"originalJsonBytes,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
	Preview           string `json:"preview,omitempty"`
	PreviewBytes      int    `json:"previewBytes,omitempty"`
}

func sanitizeCapturedStateForPersistence(step CapturedStateUnit) CapturedStateUnit {
	step.Input = sanitizeCapturedPayload(step.Input)
	step.Output = sanitizeCapturedPayload(step.Output)
	step.Error.Error = capStringForPersistence(step.Error.Error, capturedErrorMaxBytes)
	return step
}

func sanitizeCapturedPayload(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return CapturedPayloadSummary{
			Truncated:    true,
			Reason:       "json_marshal_error: " + capStringForPersistence(err.Error(), 512),
			OriginalType: typeName(v),
		}
	}
	if len(raw) <= capturedPayloadMaxJSONBytes {
		return v
	}
	sum := sha256.Sum256(raw)
	preview := previewBytes(raw, capturedPayloadPreviewBytes)
	return CapturedPayloadSummary{
		Truncated:         true,
		Reason:            "payload_exceeds_limit",
		OriginalType:      typeName(v),
		OriginalJSONBytes: len(raw),
		SHA256:            hex.EncodeToString(sum[:]),
		Preview:           preview,
		PreviewBytes:      len([]byte(preview)),
	}
}

func capStringForPersistence(s string, maxBytes int) string {
	if maxBytes <= 0 || len([]byte(s)) <= maxBytes {
		return s
	}
	raw := []byte(s)
	sum := sha256.Sum256(raw)
	preview := previewBytes(raw, maxBytes)
	return fmt.Sprintf("%s... [truncated original_bytes=%d sha256=%s]", preview, len(raw), hex.EncodeToString(sum[:]))
}

func previewBytes(raw []byte, maxBytes int) string {
	if maxBytes > 0 && len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	return strings.ToValidUTF8(string(raw), "?")
}

func typeName(v any) string {
	if v == nil {
		return ""
	}
	t := reflect.TypeOf(v)
	if t == nil {
		return ""
	}
	return t.String()
}
