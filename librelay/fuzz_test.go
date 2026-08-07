package librelay_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/contenox/contenox/librelay"
)

// FuzzReadFrame drives the decoder with arbitrary bytes. The decoder is the
// only code in this repository that runs on input chosen by whatever is on the
// far end of a relay connection, so its contract is stated as properties rather
// than examples:
//
//  1. It never panics, on any input — malformed, truncated, oversized, or
//     valid-JSON-but-nonsense.
//  2. It never amplifies: the bytes it hands back across a whole stream stay
//     bounded by the bytes it was given, so no input can make a receiver
//     allocate more than the sender spent. (The absolute per-frame ceiling is
//     asserted separately by TestUnit_ReaderOversizeIsTerminal and
//     TestUnit_ReaderDoesNotAllocatePastTheLimit, which do not need a fuzzer.)
//  3. Every frame it accepts is one the writer will accept back, so a frame
//     cannot be received-but-unforwardable — that asymmetry would strand
//     traffic at whichever hop is stricter than the one before it.
//  4. Accepting a frame implies re-encode/re-decode is stable, which is what
//     makes a relay's store-and-forward hop lossless.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte(`{"type":"relay.heartbeat","instance":"i1","id":"h1"}` + "\n"))
	f.Add([]byte(`{"type":"acp.message","instance":"i1","session":"s1","payload":{"jsonrpc":"2.0","id":1}}` + "\n"))
	f.Add([]byte(`{"type":"relay.hello","id":"1","payload":{"protocol_version":1,"instance":"i1"}}` + "\n"))
	f.Add([]byte(`{"type":"invented.later","instance":"i1","re":"9","payload":[1,2,3]}` + "\n"))
	f.Add([]byte(`{"type":"acp.message","instance":"i1","payload":"a < b & c > d"}` + "\n"))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(`{"type":`))
	f.Add([]byte(`{"type":"acp.message","instance":"i1"`))
	f.Add([]byte("{\"type\":\"x\",\"instance\":\"i\\u0000\"}\n"))
	f.Add([]byte(`{"type":"x","session":"s"}` + "\n"))
	f.Add([]byte(`{"type":"x","id":"1","re":"2"}` + "\n"))
	f.Add([]byte(`{"type":"acp.message","instance":"i1","payload":` + strings.Repeat("[", 2000) + strings.Repeat("]", 2000) + "}\n"))
	f.Add([]byte(`{"type":"acp.message","instance":"i1","payload":{"payload":{"type":"nested"}}}` + "\n"))
	f.Add([]byte("{}\n{}\n{}\n"))
	// Invalid UTF-8 inside a string: the decoder expands each bad byte to
	// U+FFFD, so a decoded frame is legitimately larger than its wire form.
	f.Add([]byte("{\"type\":\"a\xcb\xcb\xcb\xcb\"}\n"))
	f.Add(bytes.Repeat([]byte("x"), 4096))

	f.Fuzz(func(t *testing.T, in []byte) {
		r := librelay.NewReader(bytes.NewReader(in))
		// Bound the loop by the input, not by the stream: a decoder bug
		// that returns a frame without consuming bytes must fail the
		// fuzz run rather than spin it forever.
		budget := len(in) + 8
		handed := 0
		for range budget {
			frame, err := r.ReadFrame()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
					errors.Is(err, librelay.ErrFrameTooLarge) || errors.Is(err, librelay.ErrReaderClosed) {
					break
				}
				continue // per-frame error; the reader stays usable
			}
			handed += len(frame.Type) + len(frame.Instance) + len(frame.Session) +
				len(frame.ID) + len(frame.ReplyTo) + len(frame.Payload)
			checkAcceptedFrame(t, frame)
		}
		// The ceiling is 3x rather than 1x because encoding/json replaces
		// each invalid UTF-8 byte inside a string with U+FFFD, which is
		// three bytes: a legitimate expansion, and a bounded one. Anything
		// above it would mean the decoder invents data.
		if max := 3*len(in) + 64; handed > max {
			t.Fatalf("decoder handed back %d bytes from %d bytes of input (ceiling %d)", handed, len(in), max)
		}
	})
}

// checkAcceptedFrame asserts properties 3 and 4 for one accepted frame.
func checkAcceptedFrame(t *testing.T, frame librelay.Frame) {
	t.Helper()
	if err := frame.Validate(); err != nil {
		t.Fatalf("ReadFrame returned a frame that fails Validate: %v (%+v)", err, frame)
	}
	var buf bytes.Buffer
	if err := librelay.NewWriter(&buf).WriteFrame(frame); err != nil {
		t.Fatalf("accepted frame cannot be re-encoded: %v (%+v)", err, frame)
	}
	if n := bytes.Count(buf.Bytes(), []byte{'\n'}); n != 1 {
		t.Fatalf("re-encoded frame carries %d newlines, want 1: %q", n, buf.String())
	}
	again, err := librelay.NewReader(bytes.NewReader(buf.Bytes())).ReadFrame()
	if err != nil {
		t.Fatalf("re-encoded frame does not decode: %v (%q)", err, buf.String())
	}
	if again.Type != frame.Type || again.Instance != frame.Instance || again.Session != frame.Session ||
		again.ID != frame.ID || again.ReplyTo != frame.ReplyTo {
		t.Fatalf("envelope changed across a hop: %+v -> %+v", frame, again)
	}
	if !sameJSON(frame.Payload, again.Payload) {
		t.Fatalf("payload changed across a hop: %s -> %s", frame.Payload, again.Payload)
	}
}

// FuzzFrameEncode fuzzes the writer's side of the contract: whatever a caller
// puts in a frame, the writer either refuses it or emits exactly one line that
// decodes back to the same envelope. Emitting two lines from one frame is the
// framing bug that would let a payload forge a frame.
func FuzzFrameEncode(f *testing.F) {
	f.Add("acp.message", "i1", "s1", "3", "", `{"a":1}`)
	f.Add("relay.hello", "", "", "1", "", `{"protocol_version":1}`)
	f.Add("acp.message", "i1", "s1", "", "3", "\"line\\nbreak\"")
	f.Add("acp.message", "i1", "s1", "", "", "{\n  \"pretty\": true\n}")
	f.Add("acp.message", "i\n1", "s1", "", "", "null")
	f.Add("", "", "", "", "", "")

	f.Fuzz(func(t *testing.T, msgType, instance, session, id, replyTo, payload string) {
		frame := librelay.Frame{Type: msgType, Instance: instance, Session: session, ID: id, ReplyTo: replyTo}
		if payload != "" {
			frame.Payload = json.RawMessage(payload)
		}
		var buf bytes.Buffer
		if err := librelay.NewWriter(&buf).WriteFrame(frame); err != nil {
			return // refusing is always allowed; emitting something wrong is not
		}
		if n := bytes.Count(buf.Bytes(), []byte{'\n'}); n != 1 {
			t.Fatalf("frame emitted %d lines: %q", n, buf.String())
		}
		got, err := librelay.NewReader(bytes.NewReader(buf.Bytes())).ReadFrame()
		if err != nil {
			t.Fatalf("writer emitted a frame its own reader rejects: %v (%q)", err, buf.String())
		}
		if got.Type != frame.Type || got.Instance != frame.Instance || got.Session != frame.Session ||
			got.ID != frame.ID || got.ReplyTo != frame.ReplyTo {
			t.Fatalf("envelope round-trip changed: %+v -> %+v", frame, got)
		}
		if !sameJSON(frame.Payload, got.Payload) {
			t.Fatalf("payload round-trip changed: %s -> %s", frame.Payload, got.Payload)
		}
	})
}

// sameJSON compares two payloads modulo insignificant whitespace, which the
// writer strips on purpose.
func sameJSON(a, b json.RawMessage) bool {
	var ab, bb bytes.Buffer
	if len(a) > 0 {
		if err := json.Compact(&ab, a); err != nil {
			return false
		}
	}
	if len(b) > 0 {
		if err := json.Compact(&bb, b); err != nil {
			return false
		}
	}
	return ab.String() == bb.String()
}
