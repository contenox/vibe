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

// ErrStreamClosed is what a write to a closed attachment reports.
var ErrStreamClosed = errors.New("relayacp: attachment stream is closed")

type stream struct {
	session  string
	instance string
	send     SendFunc

	in   chan json.RawMessage
	done chan struct{}

	closeOnce sync.Once

	pending  []byte
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

func (s *stream) offer(payload json.RawMessage) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	// Never blocks: a full or closed queue drops the payload rather than
	// waiting on the connector's read loop.
	select {
	case s.in <- payload:
		return true
	default:
		return false
	}
}

func (s *stream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		// Closure checked before the queue: a dropped attachment stops
		// immediately rather than serving a message whose answer cannot leave.
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

func (s *stream) Write(p []byte) (int, error) {
	select {
	case <-s.done:
		return 0, ErrStreamClosed
	default:
	}
	// Refused rather than truncated: a partial JSON-RPC message would be
	// syntactically broken on the wire.
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
			// Copied out: the frame outlives this call once queued for the
			// connector to encode later.
			payload := make(json.RawMessage, len(line))
			copy(payload, line)
			// ID/ReplyTo unset: correlation lives in the JSON-RPC message,
			// not the frame.
			if err := s.send(librelay.Frame{
				Type:     librelay.TypeACPMessage,
				Instance: s.instance,
				Session:  s.session,
				Payload:  payload,
			}); err != nil {
				return 0, err
			}
		}
		// Compacted in place so a long-lived attachment's buffer does not
		// grow unbounded.
		s.outbound = append(s.outbound[:0], s.outbound[i+1:]...)
	}
}

func (s *stream) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return nil
}

var _ io.ReadWriteCloser = (*stream)(nil)
