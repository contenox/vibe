package relayacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/contenox/contenox/librelay"
)

// ErrStreamClosed is what a write to a closed attachment reports. It reaches
// libacp as an ordinary transport failure, which is what ends that attachment's
// connection; nothing else is expected to inspect it.
var ErrStreamClosed = errors.New("relayacp: attachment stream is closed")

// stream is the io.ReadWriteCloser one attachment hands to libacp: relay frames
// on one side, NDJSON on the other.
//
// # The newline is the whole contract
//
// libacp writes one message as two calls — the JSON bytes, then a lone "\n" —
// and reads through a bufio.Reader that may ask for less than a whole line. So
// Write buffers until it observes a newline and emits exactly one frame per
// line, and Read hands back one queued payload at a time, newline-terminated,
// keeping any remainder for the next call. That is what makes "one frame, one
// message" true in both directions rather than only in the direction that was
// tested.
//
// # Concurrency
//
// Read is called only by the connection's reader goroutine and Write only under
// libacp's writer mutex, so pending and outbound need no lock of their own.
// Close may be called from anywhere and at any time — it is how an attachment
// is dropped from the connector's read loop — so it touches nothing but the
// done channel.
type stream struct {
	session  string
	instance string
	send     SendFunc

	in   chan json.RawMessage
	done chan struct{}

	closeOnce sync.Once

	// pending is the unread tail of the payload Read is currently serving.
	// Reader-goroutine only.
	pending []byte
	// outbound accumulates written bytes until a newline completes a message.
	// Writer-goroutine only.
	outbound []byte
}

func newStream(session, instance string, queue int, send SendFunc) *stream {
	return &stream{
		session:  session,
		instance: instance,
		send:     send,
		in:       make(chan json.RawMessage, queue),
		done:     make(chan struct{}),
	}
}

// offer queues one inbound payload and reports whether it was taken. It never
// blocks: false means the attachment is closed or is not draining, and the
// caller drops the payload rather than waiting, because the caller is the
// connector's read loop.
func (s *stream) offer(payload json.RawMessage) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.in <- payload:
		return true
	default:
		return false
	}
}

// Read serves the next inbound message, blocking until one arrives or the
// attachment closes. A closed attachment reports [io.EOF], which is how libacp
// learns the client is gone.
//
// Closure is tested before the queue so a dropped attachment stops immediately
// rather than first serving messages whose answers can no longer leave.
func (s *stream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		select {
		case <-s.done:
			return 0, io.EOF
		default:
		}
		select {
		case msg := <-s.in:
			s.pending = make([]byte, 0, len(msg)+1)
			s.pending = append(append(s.pending, msg...), '\n')
		case <-s.done:
			return 0, io.EOF
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// Write emits one frame per completed line, echoing the attachment's Session
// unchanged — the two-end invariant the package doc states. A partial line is
// held until its newline arrives.
//
// Frame.ID and Frame.ReplyTo are deliberately never set. Correlation lives
// inside the JSON-RPC message and belongs to the two ACP endpoints; a
// frame-level request id would oblige an answer this tunnel does not produce.
//
// A message that would not fit the envelope is refused rather than truncated,
// since emitting a prefix of it would put a syntactically broken JSON-RPC
// message on the wire.
//
// Each payload is copied out of the buffer, which is then compacted in place:
// the frame outlives this call because the connector queues it and encodes it
// later, and compacting keeps a long-lived attachment's buffer from walking off
// the end of its allocation.
//
// A send failure is returned rather than swallowed: the relay is down or is not
// draining this connection, and an attachment whose answers cannot leave is
// better ended than left producing them.
func (s *stream) Write(p []byte) (int, error) {
	select {
	case <-s.done:
		return 0, ErrStreamClosed
	default:
	}
	if len(s.outbound)+len(p) > librelay.MaxFrameBytes {
		return 0, fmt.Errorf("relayacp: outbound message exceeds librelay.MaxFrameBytes (%d)", librelay.MaxFrameBytes)
	}
	s.outbound = append(s.outbound, p...)
	for {
		i := bytes.IndexByte(s.outbound, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := s.outbound[:i]
		if len(bytes.TrimSpace(line)) > 0 {
			payload := make(json.RawMessage, len(line))
			copy(payload, line)
			if err := s.send(librelay.Frame{
				Type:     librelay.TypeACPMessage,
				Instance: s.instance,
				Session:  s.session,
				Payload:  payload,
			}); err != nil {
				return 0, err
			}
		}
		s.outbound = append(s.outbound[:0], s.outbound[i+1:]...)
	}
}

// Close releases both directions. It is idempotent, does no I/O, and is
// therefore safe to call from the connector's read loop, which must never
// block.
func (s *stream) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

var _ io.ReadWriteCloser = (*stream)(nil)
