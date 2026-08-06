package libacp

import (
	"bytes"
	"strings"
	"testing"
)

func TestUnit_NDJSONReader_AllowsLargeJSONRPCFrames(t *testing.T) {
	const payloadLen = 20 * 1024 * 1024
	input := strings.Repeat("a", payloadLen) + "\n"

	got, err := newNDJSONReader(strings.NewReader(input)).Next()
	if err != nil {
		t.Fatalf("Next returned error for large frame: %v", err)
	}
	if len(got) != payloadLen {
		t.Fatalf("frame length = %d, want %d", len(got), payloadLen)
	}
}

func TestUnit_WireDump_TruncatesLargePayloads(t *testing.T) {
	var buf bytes.Buffer
	payload := bytes.Repeat([]byte("x"), maxWireDumpBytes+1024)

	wireMu.Lock()
	oldWireOut := wireOut
	wireOut = &buf
	wireMu.Unlock()
	t.Cleanup(func() {
		wireMu.Lock()
		wireOut = oldWireOut
		wireMu.Unlock()
	})

	wireDump("<-", payload)

	out := buf.String()
	if !strings.Contains(out, wireDumpTruncated) {
		t.Fatalf("wire dump did not report truncation: %q", out)
	}
	if !strings.Contains(out, "original_bytes=263168") {
		t.Fatalf("wire dump did not report original size: %q", out)
	}
	if buf.Len() > maxWireDumpBytes+512 {
		t.Fatalf("wire dump length = %d, expected near cap %d", buf.Len(), maxWireDumpBytes)
	}
}
