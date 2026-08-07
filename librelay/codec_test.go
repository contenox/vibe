package librelay_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/contenox/contenox/librelay"
)

func TestUnit_CodecRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame librelay.Frame
	}{
		{"heartbeat", librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "i1", ID: "h1"}},
		{"ack", librelay.Frame{Type: librelay.TypeAck, Instance: "i1", ReplyTo: "h1"}},
		{"acp cargo", librelay.Frame{
			Type: librelay.TypeACPMessage, Instance: "i1", Session: "sess-1",
			Payload: json.RawMessage(`{"jsonrpc":"2.0","id":3,"method":"session/prompt"}`),
		}},
		{"unknown type survives", librelay.Frame{Type: "invented.later", Instance: "i1", Payload: json.RawMessage(`[1,"two",null]`)}},
		{"unicode identifiers", librelay.Frame{Type: librelay.TypeACPMessage, Instance: "инстанс", Session: "セッション"}},
		{"html-ish payload is not escaped", librelay.Frame{
			Type: librelay.TypeACPMessage, Instance: "i1", Session: "s1",
			Payload: json.RawMessage(`{"text":"a < b && c > d"}`),
		}},
		{"empty payload", librelay.Frame{Type: librelay.TypeWelcome, Instance: "i1", ReplyTo: "1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := librelay.NewWriter(&buf).WriteFrame(tc.frame); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			if n := bytes.Count(buf.Bytes(), []byte{'\n'}); n != 1 {
				t.Fatalf("frame wrote %d newlines, want exactly 1: %q", n, buf.String())
			}
			got, err := librelay.NewReader(&buf).ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			assertSameFrame(t, got, tc.frame)
		})
	}
}

// TestUnit_CodecPayloadIsNotParsed asserts the property the whole design rests
// on: a relay moves a payload it has no schema for, byte for byte.
func TestUnit_CodecPayloadIsNotParsed(t *testing.T) {
	t.Parallel()
	payload := json.RawMessage(`{"unknown_method":"x","nested":{"deep":[{"k":"<&>"}]},"n":1e3}`)
	var buf bytes.Buffer
	if err := librelay.NewWriter(&buf).WriteFrame(librelay.Frame{
		Type: "acp.message", Instance: "i1", Session: "s1", Payload: payload,
	}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := librelay.NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(got.Payload) != string(payload) {
		t.Fatalf("payload = %s, want %s", got.Payload, payload)
	}
}

func TestUnit_ReaderRejectsBadFramesAndKeepsGoing(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"type":"relay.heartbeat","instance":"i1","id":"1"}`,
		`{"type":`,                              // truncated JSON
		``,                                      // blank line, skipped
		`{"instance":"i1"}`,                     // no type
		`{"type":"acp.message","session":"s1"}`, // session without instance
		`  `,                                    // whitespace only, skipped
		`{"type":"acp.message","instance":"i1","id":"2"}`, // still readable
	}, "\n") + "\n"

	r := librelay.NewReader(strings.NewReader(stream))
	first, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("first ReadFrame: %v", err)
	}
	if first.ID != "1" {
		t.Fatalf("first frame = %+v", first)
	}
	for i, want := range []error{nil, librelay.ErrEmptyType, librelay.ErrSessionAlone} {
		_, err := r.ReadFrame()
		if err == nil {
			t.Fatalf("bad frame %d decoded without error", i)
		}
		if want != nil && !errors.Is(err, want) {
			t.Fatalf("bad frame %d error = %v, want %v", i, err, want)
		}
	}
	last, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame after bad frames: %v", err)
	}
	if last.ID != "2" {
		t.Fatalf("last frame = %+v", last)
	}
	if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame at end = %v, want io.EOF", err)
	}
}

// TestUnit_ReaderTruncatedTailIsNotAFrame guards the case a dropped connection
// produces: the writer always emits the delimiter, so a final line without one
// is half a message and must not be handed to a caller as a whole one.
func TestUnit_ReaderTruncatedTailIsNotAFrame(t *testing.T) {
	t.Parallel()
	r := librelay.NewReader(strings.NewReader(`{"type":"acp.message","instance":"i1"`))
	if _, err := r.ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestUnit_ReaderOversizeIsTerminal asserts the memory contract and the refusal
// to resynchronize. Resyncing on a newline chosen by whoever sent the oversized
// line would let them decide where the next frame begins.
func TestUnit_ReaderOversizeIsTerminal(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	b.WriteString(`{"type":"acp.message","instance":"i1","session":"s1","payload":"`)
	b.Write(bytes.Repeat([]byte("x"), librelay.MaxFrameBytes))
	b.WriteString("\"}\n")
	b.WriteString(`{"type":"relay.heartbeat","instance":"i1","id":"1"}` + "\n")

	r := librelay.NewReader(&b)
	if _, err := r.ReadFrame(); !errors.Is(err, librelay.ErrFrameTooLarge) {
		t.Fatalf("ReadFrame = %v, want ErrFrameTooLarge", err)
	}
	if _, err := r.ReadFrame(); !errors.Is(err, librelay.ErrReaderClosed) {
		t.Fatalf("ReadFrame after oversize = %v, want ErrReaderClosed", err)
	}
}

// TestUnit_ReaderDoesNotAllocatePastTheLimit is the memory contract stated as
// a measurement: a stream far larger than one frame must not persuade the
// reader to hold more than one frame's worth. Without the pre-append check in
// readLine this fails by roughly the size of the input.
func TestUnit_ReaderDoesNotAllocatePastTheLimit(t *testing.T) {
	if testing.Short() {
		// The stream has to exceed MaxFrameBytes to mean anything, so
		// this is a memory-heavy test by construction.
		t.Skip("allocates several MiB; skipped under -short")
	}
	// The property is independence, not a constant: growing the stream
	// eightfold must not grow what the reader allocates, because it gives
	// up at MaxFrameBytes either way. An absolute budget would only be
	// measuring append's growth factor, which is not ours to promise.
	small := allocsReadingUnterminated(t, 4*librelay.MaxFrameBytes)
	large := allocsReadingUnterminated(t, 32*librelay.MaxFrameBytes)
	if large > small*3/2 {
		t.Fatalf("allocations track input length: %d bytes for a 4x stream, %d for a 32x stream", small, large)
	}
}

// allocsReadingUnterminated reports the bytes allocated while the reader
// refuses a stream that never terminates a line.
func allocsReadingUnterminated(t *testing.T, streamBytes int) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := librelay.NewReader(&endlessNonNewline{remaining: streamBytes})
	if _, err := r.ReadFrame(); !errors.Is(err, librelay.ErrFrameTooLarge) {
		t.Fatalf("ReadFrame over %d bytes = %v, want ErrFrameTooLarge", streamBytes, err)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(r)
	return after.TotalAlloc - before.TotalAlloc
}

// endlessNonNewline yields bytes that never terminate a line, which is the
// cheapest way for a peer to ask a decoder to buffer without bound.
type endlessNonNewline struct{ remaining int }

func (e *endlessNonNewline) Read(p []byte) (int, error) {
	if e.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), e.remaining)
	for i := range n {
		p[i] = 'x'
	}
	e.remaining -= n
	return n, nil
}

func TestUnit_WriterFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame librelay.Frame
	}{
		{"invalid frame", librelay.Frame{Instance: "i1"}},
		{"payload is not json", librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Payload: json.RawMessage(`{oops`)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := librelay.NewWriter(&buf).WriteFrame(tc.frame); err == nil {
				t.Fatal("WriteFrame accepted an unwritable frame")
			}
			if buf.Len() != 0 {
				t.Fatalf("WriteFrame emitted %d bytes for a rejected frame", buf.Len())
			}
		})
	}
}

// TestUnit_WriterIsAtomicUnderConcurrency asserts the invariant that makes one
// connection shareable: interleaved writes would corrupt framing for every
// session on it, not only the one that raced.
func TestUnit_WriterIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	var buf lockedBuffer
	w := librelay.NewWriter(&buf)
	const writers, each = 8, 64
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range each {
				f := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Session: "s1"}
				f, _ = f.WithPayload(map[string]int{"w": i, "n": j})
				if err := w.WriteFrame(f); err != nil {
					t.Errorf("WriteFrame: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	r := librelay.NewReader(bytes.NewReader(buf.b.Bytes()))
	seen := 0
	for {
		f, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame after %d frames: %v", seen, err)
		}
		var got map[string]int
		if err := f.DecodePayload(&got); err != nil {
			t.Fatalf("frame %d payload: %v", seen, err)
		}
		seen++
	}
	if seen != writers*each {
		t.Fatalf("read %d frames, wrote %d", seen, writers*each)
	}
}

// lockedBuffer serializes only the underlying Write, so any interleaving the
// Writer allows would still show up as a corrupt stream.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func assertSameFrame(t *testing.T, got, want librelay.Frame) {
	t.Helper()
	if got.Type != want.Type || got.Instance != want.Instance || got.Session != want.Session ||
		got.ID != want.ID || got.ReplyTo != want.ReplyTo {
		t.Fatalf("envelope = %+v, want %+v", got, want)
	}
	var gotPayload, wantPayload bytes.Buffer
	if len(got.Payload) > 0 {
		if err := json.Compact(&gotPayload, got.Payload); err != nil {
			t.Fatalf("decoded payload is not json: %v", err)
		}
	}
	if len(want.Payload) > 0 {
		if err := json.Compact(&wantPayload, want.Payload); err != nil {
			t.Fatalf("source payload is not json: %v", err)
		}
	}
	if gotPayload.String() != wantPayload.String() {
		t.Fatalf("payload = %s, want %s", gotPayload.String(), wantPayload.String())
	}
}
