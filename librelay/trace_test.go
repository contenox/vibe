package librelay_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/librelay"
)

// TestUnit_ValidTraceID states the alphabet as cases rather than by restating
// the constant, so a widening of it has to be argued for here too.
func TestUnit_ValidTraceID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		trace string
		want  bool
	}{
		{"empty is untraced, not invalid", "", true},
		{"a minted id", librelay.NewTraceID(), true},
		{"letters digits dash underscore", "Ab9-_z", true},
		{"at the ceiling", strings.Repeat("a", librelay.MaxTraceBytes), true},
		{"one byte past the ceiling", strings.Repeat("a", librelay.MaxTraceBytes+1), false},
		{"space", "tr-a b", false},
		{"quote", `tr-"a`, false},
		{"path separator", "tr-a/b", false},
		{"newline", "tr-a\nb", false},
		{"nul", "tr-a\x00b", false},
		{"non-ascii", "tr-é", false},
		{"invalid utf-8", "tr-\xcb\xcb", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := librelay.ValidTraceID(tc.trace); got != tc.want {
				t.Fatalf("ValidTraceID(%q) = %v, want %v", tc.trace, got, tc.want)
			}
		})
	}
}

// TestUnit_TraceAlphabetMatchesTheScanner holds the two spellings of the
// alphabet — the exported constant and the byte ranges ValidTraceID scans — to
// the same answer for every possible byte. They are stated twice for speed, and
// two statements of one rule drift silently unless something checks: a widened
// constant with an unwidened scanner would document an alphabet the code
// rejects, and the reverse would admit a byte the documentation forbids.
func TestUnit_TraceAlphabetMatchesTheScanner(t *testing.T) {
	t.Parallel()
	for b := 0; b < 256; b++ {
		s := string([]byte{byte(b)})
		inConstant := strings.IndexByte(librelay.TraceAlphabet, byte(b)) >= 0
		if got := librelay.ValidTraceID(s); got != inConstant {
			t.Fatalf("byte %#02x: ValidTraceID = %v, TraceAlphabet contains it = %v", b, got, inConstant)
		}
	}
}

// TestUnit_NewTraceIDIsAcceptableAndDistinct pins the only two properties a
// correlation key has to have: it survives the validation the receiving end
// applies, and two of them are not the same key.
func TestUnit_NewTraceIDIsAcceptableAndDistinct(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 512)
	for range 512 {
		id := librelay.NewTraceID()
		if !librelay.ValidTraceID(id) {
			t.Fatalf("NewTraceID minted %q, which ValidTraceID rejects", id)
		}
		f := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Trace: id}
		if err := f.Validate(); err != nil {
			t.Fatalf("a minted trace fails Validate: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewTraceID repeated %q within 512 draws", id)
		}
		seen[id] = struct{}{}
	}
}

// TestUnit_FrameValidateRejectsAnUnloggableTrace covers the "unsafe to log"
// half of Validate's rule. Length is reported ahead of the alphabet so an
// oversized value says how big it was rather than naming an arbitrary byte.
func TestUnit_FrameValidateRejectsAnUnloggableTrace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		trace string
		want  error
	}{
		{"untraced", "", nil},
		{"minted", librelay.NewTraceID(), nil},
		{"too long", strings.Repeat("a", librelay.MaxTraceBytes+1), librelay.ErrTraceTooLong},
		{"too long and malformed reports the length", strings.Repeat(" ", librelay.MaxTraceBytes+1), librelay.ErrTraceTooLong},
		{"space", "tr- ", librelay.ErrTraceCharset},
		{"control character", "tr-\x01", librelay.ErrTraceCharset},
		{"not utf-8", "tr-\xcb", librelay.ErrTraceCharset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Trace: tc.trace}
			err := f.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUnit_TraceSurvivesAHopAndAnUnloggableOneDoesNotCross asserts the two
// halves of the carrier's contract at once: a well-formed trace is the same
// value on the far side of an encode/decode, and one that is not well-formed
// never reaches a decoded frame at all — not stripped and delivered, which
// would leave the sender believing an action is traced when nothing downstream
// can join on it.
func TestUnit_TraceSurvivesAHopAndAnUnloggableOneDoesNotCross(t *testing.T) {
	t.Parallel()

	trace := librelay.NewTraceID()
	sent := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Session: "s1", Trace: trace}
	var buf bytes.Buffer
	if err := librelay.NewWriter(&buf).WriteFrame(sent); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"trace":"`+trace+`"`)) {
		t.Fatalf("the wire form does not carry the trace: %q", buf.String())
	}
	got, err := librelay.NewReader(bytes.NewReader(buf.Bytes())).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Trace != trace {
		t.Fatalf("Trace = %q after a hop, want %q", got.Trace, trace)
	}

	// An untraced frame stays untraced and spends no bytes saying so.
	buf.Reset()
	if err := librelay.NewWriter(&buf).WriteFrame(librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "i1", ID: "h1"}); err != nil {
		t.Fatalf("WriteFrame untraced: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"trace"`)) {
		t.Fatalf("an untraced frame carries a trace field: %q", buf.String())
	}

	// The writer refuses to emit an unloggable trace, so a peer using this
	// package cannot produce one...
	bad := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Trace: "tr- oops"}
	if err := librelay.NewWriter(&bytes.Buffer{}).WriteFrame(bad); !errors.Is(err, librelay.ErrTraceCharset) {
		t.Fatalf("WriteFrame(bad trace) = %v, want ErrTraceCharset", err)
	}
	// ...and a peer that produced one some other way has its frame rejected
	// rather than quietly stripped of the field.
	line := []byte(`{"type":"acp.message","instance":"i1","trace":"tr- oops"}` + "\n" +
		`{"type":"acp.message","instance":"i1","trace":"` + strings.Repeat("a", librelay.MaxTraceBytes+1) + `"}` + "\n" +
		`{"type":"acp.message","instance":"i1","session":"s-after"}` + "\n")
	r := librelay.NewReader(bytes.NewReader(line))
	if _, err := r.ReadFrame(); !errors.Is(err, librelay.ErrTraceCharset) {
		t.Fatalf("ReadFrame(bad charset) = %v, want ErrTraceCharset", err)
	}
	if _, err := r.ReadFrame(); !errors.Is(err, librelay.ErrTraceTooLong) {
		t.Fatalf("ReadFrame(oversize) = %v, want ErrTraceTooLong", err)
	}
	// Per-frame, not per-connection: the reader is still usable, which is
	// what keeps one peer's bad trace from stranding every session on the
	// link.
	after, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame after two rejected traces: %v", err)
	}
	if after.Session != "s-after" || after.Trace != "" {
		t.Fatalf("frame after the rejected ones = %+v", after)
	}
}
